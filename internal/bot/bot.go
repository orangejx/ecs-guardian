// Package bot 实现 Telegram 控制机器人（对应上游 ecs_bot.py 的等价功能）：
// 管理员白名单、实例列表、状态查询、远程开机/关机/重启、定时任务。
package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/orangejx/ecs-guardian/internal/aliyun"
	"github.com/orangejx/ecs-guardian/internal/config"
)

// 用户界面文案保持与上游接近，方便已有用户迁移。
const (
	helpText = "/start - 检查机器人\n" +
		"/menu - 交互菜单\n" +
		"/list - 实例列表\n" +
		"/status <实例名或ID> - 查询状态\n" +
		"/start_instance <实例名或ID> - 开机\n" +
		"/stop <实例名或ID> - 关机\n" +
		"/reboot <实例名或ID> - 重启\n" +
		"/timers - 查看定时任务"
)

// Timer 是持久化的定时任务（写入 bot_state.json）。
type Timer struct {
	Action    string `json:"action"` // start / stop
	Time      string `json:"time"`   // "HH:MM"
	CreatedAt string `json:"created_at"`
}

// State 机器人持久化状态。
type State struct {
	Timers map[string][]Timer `json:"timers"`
}

// Bot 控制机器人实例。
type Bot struct {
	cfg      *config.Config
	botAPI   *tgbotapi.BotAPI
	mu       sync.Mutex
	state    *State
	statePath string
}

// apiEndpoint 常量，与 tgbotapi.APIEndpoint 保持一致（https://api.telegram.org/bot%s/%s）。
// 单独定义常量是为了在下面构造自定义 transport 的 BotAPI 时复用。
const apiEndpoint = "https://api.telegram.org/bot%s/%s"

// loggableClient 是一个 HTTPClient 包装：记录 Telegram 请求的 URL/方法，
// 以及失败时的响应状态与响应体（用于排查"为什么收不到消息/update EOF"）。
type loggableClient struct {
	inner   *http.Client
	botName string
}

// logBody 读取并记录响应体（只读一次，记录后不影响调用方使用原始流）。
func (c *loggableClient) Do(req *http.Request) (*http.Response, error) {
	resp, err := c.inner.Do(req)
	if err != nil {
		// 网络层错误（EOF / 超时 / 连接重置等）：记录并返回，调用方照常处理
		log.Printf("[bot:%s] %s %s 请求失败: %v", c.botName, req.Method, req.URL, err)
		return resp, err
	}
	// 读取响应体并原样恢复，便于调用方 json 解码
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	log.Printf("[bot:%s] %s %s -> HTTP %d body=%s",
		c.botName, req.Method, req.URL, resp.StatusCode, truncateBody(body))
	return resp, nil
}

// truncateBody 截断响应体用于日志（避免超长 JSON 刷屏）。
func truncateBody(b []byte) string {
	const max = 600
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + fmt.Sprintf("...(共 %d 字节)", len(b))
}

// New 创建机器人。botToken 为空或未配置 admin_users 时返回错误（由调用方决定是否启动）。
func New(cfg *config.Config) (*Bot, error) {
	token := strings.TrimSpace(cfg.Telegram.BotToken)
	if token == "" {
		return nil, fmt.Errorf("bot_token 未配置")
	}
	if len(cfg.AdminUsers) == 0 {
		return nil, fmt.Errorf("admin_users 未配置，不启动控制机器人")
	}

	// 自定义 transport：为每次请求设置明确超时，并绑定系统代理环境变量。
	// 默认的 http.DefaultTransport 在 Telegram 长时间无消息(长轮询挂起)时，
	// 网络链路中断可能表现为 "unexpected EOF"，此时应能按配置重试。
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        10,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	httpClient := &http.Client{Transport: transport, Timeout: 90 * time.Second}

	// NewBotAPIWithClient：自行传入 http.Client，并用 loggableClient 包装以记录请求/响应
	api, err := tgbotapi.NewBotAPIWithClient(token, apiEndpoint, &loggableClient{inner: httpClient, botName: token[:8]})
	if err != nil {
		return nil, fmt.Errorf("Telegram API 初始化失败: %w", err)
	}
	statePath := filepath.Join(config.DefaultDataDir(), "bot_state.json")
	b := &Bot{
		cfg:       cfg,
		botAPI:    api,
		statePath: statePath,
		state:     &State{Timers: map[string][]Timer{}},
	}
	b.loadState()
	return b, nil
}

func (b *Bot) loadState() {
	data, err := os.ReadFile(b.statePath)
	if err != nil {
		return
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		log.Printf("[bot] 状态文件解析失败: %v", err)
		return
	}
	if st.Timers == nil {
		st.Timers = map[string][]Timer{}
	}
	b.state = &st
}

func (b *Bot) saveState() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	data, err := json.MarshalIndent(b.state, "", "  ")
	if err != nil {
		return err
	}
	tmp := b.statePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, b.statePath)
}

// isAuthorized 检查 Telegram 用户是否为管理员。
func (b *Bot) isAuthorized(uid int64) bool {
	for _, id := range b.cfg.AdminUsers {
		if id == uid {
			return true
		}
	}
	return false
}

// findUser 按 备注名 / 实例ID / 备注名前缀 查找用户配置。
func (b *Bot) findUser(identifier string) *config.UserConfig {
	ident := strings.TrimSpace(identifier)
	if ident == "" {
		return nil
	}
	for i := range b.cfg.Users {
		u := &b.cfg.Users[i]
		if u.InstanceID == ident || u.Name == ident {
			return u
		}
	}
	lower := strings.ToLower(ident)
	for i := range b.cfg.Users {
		u := &b.cfg.Users[i]
		if u.Name != "" && strings.ToLower(u.Name) == lower {
			return u
		}
	}
	return nil
}

// Run 启动轮询，阻塞直到 ctx 取消。以单独 goroutine 检查定时任务。
func (b *Bot) Run(ctx context.Context) error {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 30
	updates := b.botAPI.GetUpdatesChan(u)

	go b.timerLoop(ctx)

	log.Printf("[bot] 控制机器人已启动，管理员: %v", b.cfg.AdminUsers)
	for {
		select {
		case <-ctx.Done():
			return nil
		case update, ok := <-updates:
			if !ok {
				return fmt.Errorf("Telegram update 通道关闭")
			}
			b.handleUpdate(update)
		}
	}
}

// handleUpdate 处理一条 Telegram 更新（命令或回调查询）。
func (b *Bot) handleUpdate(update tgbotapi.Update) {
	if update.CallbackQuery != nil {
		b.handleCallback(update.CallbackQuery)
		return
	}
	if update.Message == nil || !update.Message.IsCommand() {
		return
	}
	if !b.isAuthorized(update.Message.From.ID) {
		msg := tgbotapi.NewMessage(update.Message.Chat.ID, "无权限。请确认 config.json 中 admin_users 已配置你的 Telegram 用户 ID。")
		b.botAPI.Send(msg)
		return
	}
	cmd := update.Message.Command()
	args := strings.TrimSpace(update.Message.CommandArguments())
	switch cmd {
	case "start":
		b.send(update.Message.Chat.ID, "阿里云 ECS 控制机器人已就绪。使用 /menu 打开交互菜单。")
	case "menu":
		b.sendListMenu(update.Message.Chat.ID)
	case "list":
		b.sendList(update.Message.Chat.ID)
	case "help":
		b.send(update.Message.Chat.ID, helpText)
	case "status":
		b.sendStatus(update.Message.Chat.ID, args)
	case "start_instance":
		b.sendAction(update.Message.Chat.ID, args, "start")
	case "stop":
		b.sendAction(update.Message.Chat.ID, args, "stop")
	case "reboot":
		b.sendAction(update.Message.Chat.ID, args, "reboot")
	case "timers":
		b.sendTimers(update.Message.Chat.ID)
	default:
		b.send(update.Message.Chat.ID, "未知命令，/help 查看帮助")
	}
}

func (b *Bot) send(chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	if _, err := b.botAPI.Send(msg); err != nil {
		log.Printf("[bot] 发送消息失败: %v", err)
	}
}

// sendList 发送实例列表。
func (b *Bot) sendList(chatID int64) {
	var lines []string
	for i, u := range b.cfg.Users {
		paused := ""
		if u.Paused || u.Disabled {
			paused = "，已暂停"
		}
		lines = append(lines, fmt.Sprintf("%d. %s (%s%s)", i+1, u.Name, u.Region, paused))
	}
	if len(lines) == 0 {
		b.send(chatID, "没有配置实例。")
		return
	}
	b.send(chatID, "实例列表:\n"+strings.Join(lines, "\n"))
}

// sendListMenu 发送带内联按钮的实例选择菜单（等价 /menu）。
func (b *Bot) sendListMenu(chatID int64) {
	if len(b.cfg.Users) == 0 {
		b.send(chatID, "没有配置实例。")
		return
	}
	var rows [][]tgbotapi.InlineKeyboardButton
	for i, u := range b.cfg.Users {
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData(u.Name, fmt.Sprintf("inst_%d", i)),
		})
	}
	kb := tgbotapi.NewInlineKeyboardMarkup(rows...)
	msg := tgbotapi.NewMessage(chatID, "请选择实例:")
	msg.ReplyMarkup = kb
	if _, err := b.botAPI.Send(msg); err != nil {
		log.Printf("[bot] 发送菜单失败: %v", err)
	}
}

// handleCallback 处理内联按钮回调查询。
func (b *Bot) handleCallback(cb *tgbotapi.CallbackQuery) {
	if !b.isAuthorized(cb.From.ID) {
		b.botAPI.Request(tgbotapi.NewCallback(cb.ID, "无权限"))
		return
	}
	chatID := cb.Message.Chat.ID
	data := cb.Data
	switch {
	case data == "back":
		b.sendListMenu(chatID)
		return
	case strings.HasPrefix(data, "inst_"):
		idx, err := strconv.Atoi(strings.TrimPrefix(data, "inst_"))
		if err != nil || idx < 0 || idx >= len(b.cfg.Users) {
			b.editText(chatID, cb.Message.MessageID, "实例序号无效。")
			return
		}
		u := &b.cfg.Users[idx]
		b.editKeyboard(chatID, cb.Message.MessageID, fmt.Sprintf("已选择: %s", u.Name), b.operationKeyboard(idx))
		return
	case strings.HasPrefix(data, "op"):
		// 形如 op0_start / op1_stop / op2_reboot / op3_status
		rest := strings.TrimPrefix(data, "op")
		us := strings.SplitN(rest, "_", 2)
		if len(us) != 2 {
			return
		}
		idx, err := strconv.Atoi(us[0])
		if err != nil || idx < 0 || idx >= len(b.cfg.Users) {
			return
		}
		u := &b.cfg.Users[idx]
		b.runOperation(chatID, u, us[1])
		return
	default:
		b.botAPI.Request(tgbotapi.NewCallback(cb.ID, ""))
	}
}

func (b *Bot) operationKeyboard(idx int) tgbotapi.InlineKeyboardMarkup {
	base := fmt.Sprintf("op%d", idx)
	rows := [][]tgbotapi.InlineKeyboardButton{
		{tgbotapi.NewInlineKeyboardButtonData("开机", base + "_start")},
		{tgbotapi.NewInlineKeyboardButtonData("关机", base + "_stop")},
		{tgbotapi.NewInlineKeyboardButtonData("重启", base + "_reboot")},
		{tgbotapi.NewInlineKeyboardButtonData("状态", base + "_status")},
		{tgbotapi.NewInlineKeyboardButtonData("返回", "back")},
	}
	return tgbotapi.NewInlineKeyboardMarkup(rows...)
}

// runOperation 执行内联按钮对应的实例操作。
func (b *Bot) runOperation(chatID int64, u *config.UserConfig, data string) {
	action := data[strings.LastIndex(data, "_")+1:]
	client, err := aliyun.NewClient(u.AK, u.SK, u.Region)
	if err != nil {
		b.send(chatID, "创建客户端失败: "+err.Error())
		return
	}
	switch action {
	case "status":
		b.sendStatus(chatID, u.Name)
	case "start":
		if err := client.StartInstance(u.InstanceID); err != nil {
			b.send(chatID, fmt.Sprintf("启动失败: %s\n%v", u.Name, err))
		} else {
			b.send(chatID, fmt.Sprintf("成功: %s\n启动命令已发送", u.Name))
		}
	case "stop":
		if err := client.StopInstance(u.InstanceID, false); err != nil {
			b.send(chatID, fmt.Sprintf("停止失败: %s\n%v", u.Name, err))
		} else {
			b.send(chatID, fmt.Sprintf("成功: %s\n停止命令已发送", u.Name))
		}
	case "reboot":
		if err := client.RebootInstance(u.InstanceID, false); err != nil {
			b.send(chatID, fmt.Sprintf("重启失败: %s\n%v", u.Name, err))
		} else {
			b.send(chatID, fmt.Sprintf("成功: %s\n重启命令已发送", u.Name))
		}
	default:
		b.send(chatID, "未知操作")
	}
}

func (b *Bot) editText(chatID int64, messageID int, text string) {
	msg := tgbotapi.NewEditMessageText(chatID, messageID, text)
	if _, err := b.botAPI.Send(msg); err != nil {
		log.Printf("[bot] 编辑消息失败: %v", err)
	}
}

func (b *Bot) editKeyboard(chatID int64, messageID int, text string, kb tgbotapi.InlineKeyboardMarkup) {
	msg := tgbotapi.NewEditMessageText(chatID, messageID, text)
	msg.ReplyMarkup = &kb
	if _, err := b.botAPI.Send(msg); err != nil {
		log.Printf("[bot] 编辑消息失败: %v", err)
	}
}

// sendStatus 查询并发送实例状态。
func (b *Bot) sendStatus(chatID int64, identifier string) {
	if strings.TrimSpace(identifier) == "" {
		b.send(chatID, "用法: /status <实例名或ID>")
		return
	}
	u := b.findUser(identifier)
	if u == nil {
		b.send(chatID, "未找到该实例。")
		return
	}
	client, err := aliyun.NewClient(u.AK, u.SK, u.Region)
	if err != nil {
		b.send(chatID, "创建客户端失败: "+err.Error())
		return
	}
	detail, err := client.GetInstanceDetail(u.InstanceID)
	if err != nil {
		b.send(chatID, fmt.Sprintf("查询实例失败: %v", err))
		return
	}
	trafficGB := -1.0
	if bytes, err := client.GetCDTInternetTraffic(aliyun.CDTQueryParams{}); err == nil {
		trafficGB = float64(bytes) / (1024 * 1024 * 1024)
	}
	balanceStr := "查询失败"
	bal, currency, err := client.QueryAccountBalance(u.BillEndpoint)
	if err == nil {
		sym := map[string]string{"CNY": "¥", "USD": "$"}[currency]
		if sym == "" {
			sym = u.Currency
		}
		balanceStr = fmt.Sprintf("%s%.2f", sym, bal)
	}
	trafficStr := "⚠️ 查询失败"
	if trafficGB >= 0 {
		trafficStr = fmt.Sprintf("%.2f GB", trafficGB)
	}
	text := fmt.Sprintf(
		"📅 时间: %s\n👤 实例: %s (%dC)\n🖥️ 状态: %s\n🌐 IP: %s\n📉 流量: %s\n💳 余额: %s",
		time.Now().Format("2006-01-02 15:04"), detail.Name, detail.CPU,
		statusIcon(detail.Status), detail.IP, trafficStr, balanceStr)
	b.send(chatID, text)
}

func statusIcon(status string) string {
	switch status {
	case "Running":
		return "🟢 " + status
	case "Stopped":
		return "⚫ " + status
	case "Starting", "Stopping":
		return "🟡 " + status
	default:
		return "❓ " + status
	}
}

// sendAction 执行开机/关机/重启命令（/start_instance /stop /reboot）。
func (b *Bot) sendAction(chatID int64, identifier, action string) {
	if strings.TrimSpace(identifier) == "" {
		usage := map[string]string{"start": "/start_instance", "stop": "/stop", "reboot": "/reboot"}[action]
		b.send(chatID, "用法: "+usage+" <实例名或ID>")
		return
	}
	u := b.findUser(identifier)
	if u == nil {
		b.send(chatID, "未找到该实例。")
		return
	}
	client, err := aliyun.NewClient(u.AK, u.SK, u.Region)
	if err != nil {
		b.send(chatID, "创建客户端失败: "+err.Error())
		return
	}
	var opErr error
	switch action {
	case "start":
		opErr = client.StartInstance(u.InstanceID)
	case "stop":
		opErr = client.StopInstance(u.InstanceID, false)
	case "reboot":
		opErr = client.RebootInstance(u.InstanceID, false)
	}
	if opErr != nil {
		b.send(chatID, fmt.Sprintf("%s失败: %s\n%v", actionText(action), u.Name, opErr))
		return
	}
	b.send(chatID, fmt.Sprintf("成功: %s\n%s命令已发送", u.Name, actionText(action)))
}

func actionText(action string) string {
	switch action {
	case "start":
		return "启动"
	case "stop":
		return "停止"
	case "reboot":
		return "重启"
	}
	return action
}

// sendTimers 发送当前定时任务列表。
func (b *Bot) sendTimers(chatID int64) {
	if len(b.state.Timers) == 0 {
		b.send(chatID, "当前没有定时任务。")
		return
	}
	userMap := map[string]string{}
	for _, u := range b.cfg.Users {
		userMap[u.InstanceID] = u.Name
	}
	var lines []string
	var keys []string
	for k := range b.state.Timers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, id := range keys {
		for _, t := range b.state.Timers[id] {
			at := actionText(t.Action)
			lines = append(lines, fmt.Sprintf("- %s: %s @ %s", userMap[id], at, t.Time))
		}
	}
	b.send(chatID, "当前定时任务:\n"+strings.Join(lines, "\n"))
}

// timerLoop 每 30 秒检查一次定时任务（分钟级精度足够）。
func (b *Bot) timerLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			b.checkTimers(now)
		}
	}
}

// checkTimers 执行到点的定时任务，并处理跨天事件。
func (b *Bot) checkTimers(now time.Time) {
	b.mu.Lock()
	timers := make(map[string][]Timer, len(b.state.Timers))
	for k, v := range b.state.Timers {
		timers[k] = v
	}
	b.mu.Unlock()

	for instanceID, list := range timers {
		u := b.findUser(instanceID)
		if u == nil {
			continue
		}
		var remaining []Timer
		changed := false
		for _, t := range list {
			hh, mm := parseTime(t.Time)
			// 每分钟触发一次；用窗口避免重复触发
			if now.Hour() == hh && now.Minute() == mm && now.Second() < 30 {
				go b.runTimer(u, t.Action)
			}
			remaining = append(remaining, t)
		}
		_ = remaining
		_ = changed
	}
}

func parseTime(s string) (int, int) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return -1, -1
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return -1, -1
	}
	return h, m
}

func (b *Bot) runTimer(u *config.UserConfig, action string) {
	client, err := aliyun.NewClient(u.AK, u.SK, u.Region)
	if err != nil {
		log.Printf("[bot] 定时任务执行失败（创建客户端）: %v", err)
		return
	}
	var opErr error
	if action == "start" {
		opErr = client.StartInstance(u.InstanceID)
	} else {
		opErr = client.StopInstance(u.InstanceID, false)
	}
	chatID, _ := strconv.ParseInt(b.cfg.Telegram.ChatID, 10, 64)
	status := "成功"
	if opErr != nil {
		status = "失败: " + opErr.Error()
	}
	text := fmt.Sprintf("⏰ 定时任务执行\n实例: %s\n操作: %s\n结果: %s", u.Name, actionText(action), status)
	if chatID != 0 {
		b.send(chatID, text)
	}
	log.Printf("[bot] 定时%s %s: %s", actionText(action), u.InstanceID, status)
}
