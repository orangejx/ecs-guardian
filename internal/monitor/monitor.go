package monitor

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/orangejx/ecs-guardian/internal/aliyun"
	"github.com/orangejx/ecs-guardian/internal/config"
	"github.com/orangejx/ecs-guardian/internal/notify"
)

// 与 Python 版 monitor.py 一致的参数
const (
	notifyCooldown        = time.Hour                          // 通用事件通知冷却：1 小时内不重复
	overlimitCooldown     = 24 * time.Hour                     // 流量超标提醒冷却：24 小时一次
	startWaitTimeout      = 180 * time.Second                  // 等待实例启动轮询超时
	startPollInterval     = 10 * time.Second                   // 启动轮询间隔
	userCheckTimeout      = startWaitTimeout + 120*time.Second // 单实例巡检硬超时
	maxStartFailures      = 3                                  // 连续启动失败达到此值后降频重试
	resourceRetryCooldown = 30 * time.Minute                   // 资源不足重试冷却
	checkFailureThreshold = 3                                  // 连续巡检失败告警阈值
)

// InstanceState 是单个实例的持久化状态（写入 monitor_state.json）。
type InstanceState struct {
	StartFailures int                  `json:"start_failures,omitempty"`
	LastRetry     time.Time            `json:"last_retry_ts,omitempty"`
	CheckFailures int                  `json:"check_failures,omitempty"`
	LastNotify    map[string]time.Time `json:"last_notify,omitempty"`
}

// StateFile 管理整个监控状态文件（并发安全：由单进程单 goroutine 访问）。
type StateFile struct {
	path  string
	state map[string]*InstanceState
}

// LoadState 读取状态文件，不存在/损坏时返回空状态。
func LoadState(dataDir string) *StateFile {
	path := filepath.Join(dataDir, "monitor_state.json")
	sf := &StateFile{path: path, state: map[string]*InstanceState{}}
	data, err := os.ReadFile(path)
	if err != nil {
		return sf
	}
	if err := json.Unmarshal(data, &sf.state); err != nil {
		log.Printf("[state] 状态文件解析失败，重置: %v", err)
		sf.state = map[string]*InstanceState{}
	}
	if sf.state == nil {
		sf.state = map[string]*InstanceState{}
	}
	return sf
}

// Save 原子写入状态文件。
func (sf *StateFile) Save() error {
	data, err := json.MarshalIndent(sf.state, "", "  ")
	if err != nil {
		return err
	}
	tmp := sf.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, sf.path)
}

func (sf *StateFile) stateFor(id string) *InstanceState {
	st, ok := sf.state[id]
	if !ok {
		st = &InstanceState{LastNotify: map[string]time.Time{}}
		sf.state[id] = st
	}
	if st.LastNotify == nil {
		st.LastNotify = map[string]time.Time{}
	}
	return st
}

// canNotify 判断某事件是否已过冷却期。
func (sf *StateFile) canNotify(id, event string, cooldown time.Duration) bool {
	st := sf.stateFor(id)
	last, ok := st.LastNotify[event]
	if !ok {
		return true
	}
	return time.Since(last) >= cooldown
}

func (sf *StateFile) markNotified(id, event string) {
	st := sf.stateFor(id)
	if st.LastNotify == nil {
		st.LastNotify = map[string]time.Time{}
	}
	st.LastNotify[event] = time.Now()
}

// RunOnce 执行一轮完整巡检（等价于 Python monitor.py 单次执行）。
func RunOnce() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	sf := LoadState(config.DefaultDataDir())
	defer sf.Save()
	notifier := notify.FromConfig(cfg)
	for i := range cfg.Users {
		user := &cfg.Users[i]
		checkUser(user, cfg, sf, notifier)
	}
	return nil
}

// checkUser 巡检单个实例，含硬超时保护（防止某个请求永久阻塞整轮巡检）。
func checkUser(user *config.UserConfig, cfg *config.Config, sf *StateFile, notifier notify.Notifier) {
	instanceID := user.InstanceID
	name := user.Name
	if name == "" {
		name = instanceID
	}
	if user.Paused || user.Disabled {
		log.Printf("[%s] 监控已暂停，跳过本轮检查", name)
		return
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			// 捕获 panic，避免单实例问题杀死整个进程
			if r := recover(); r != nil {
				log.Printf("[%s] 巡检 panic: %v", name, r)
				st := sf.stateFor(instanceID)
				st.CheckFailures++
				alertCheckFailure(user, cfg, sf, notifier, fmt.Sprintf("panic: %v", r))
			}
		}()
		checkAndAct(user, cfg, sf, notifier)
	}()

	select {
	case <-done:
	case <-time.After(userCheckTimeout):
		log.Printf("[%s] 单实例巡检超过 %s，已跳过本实例", name, userCheckTimeout)
		st := sf.stateFor(instanceID)
		st.CheckFailures++
	}
}

// checkAndAct 执行单实例的查询-决策-动作逻辑。
func checkAndAct(user *config.UserConfig, cfg *config.Config, sf *StateFile, notifier notify.Notifier) {
	instanceID := user.InstanceID
	name := user.Name
	if name == "" {
		name = instanceID
	}

	client, err := aliyun.NewClient(user.AK, user.SK, user.Region)
	if err != nil {
		log.Printf("[%s] 创建客户端失败: %v", name, err)
		st := sf.stateFor(instanceID)
		st.CheckFailures++
		alertCheckFailure(user, cfg, sf, notifier, err.Error())
		return
	}

	// 1. 查询 CDT 流量（全局）
	trafficBytes, err := client.GetCDTInternetTraffic(aliyun.CDTQueryParams{})
	if err != nil {
		log.Printf("[%s] 查询流量失败: %v", name, err)
		st := sf.stateFor(instanceID)
		st.CheckFailures++
		alertCheckFailure(user, cfg, sf, notifier, err.Error())
		return
	}
	currGB := float64(trafficBytes) / (1024 * 1024 * 1024)

	// 2. 查询实例状态
	status, err := client.GetInstanceStatus(instanceID)
	if err != nil {
		log.Printf("[%s] 查询实例状态失败: %v", name, err)
		st := sf.stateFor(instanceID)
		st.CheckFailures++
		alertCheckFailure(user, cfg, sf, notifier, err.Error())
		return
	}

	// 3. 流量与状态都查询成功，重置连续巡检失败计数
	st := sf.stateFor(instanceID)
	st.CheckFailures = 0

	limit := user.TrafficLimit
	if limit <= 0 {
		limit = 180
	}

	switch {
	case currGB < limit:
		// ---- 流量安全 ----
		if status == "Stopped" {
			tryStartInstance(user, cfg, client, sf, notifier, currGB, status)
		} else if status == "Running" {
			st.StartFailures = 0
			log.Printf("[%s] 流量安全(%.2fGB)，实例运行中", name, currGB)
		} else {
			log.Printf("[%s] 实例处于中间态: %s，不干预", name, status)
		}

	case currGB >= limit:
		// ---- 流量超标 ----
		if status == "Running" {
			log.Printf("[%s] 流量超标(%.2fGB >= %.2fGB)，正在停止...", name, currGB, limit)
			if err := client.StopInstance(instanceID, false); err != nil {
				log.Printf("[%s] 停止实例失败: %v", name, err)
			} else if sf.canNotify(instanceID, "overlimit", overlimitCooldown) {
				msg := fmt.Sprintf("机器: %s\n当前流量: %.2fGB\n动作: 已触发止损关机 🛑",
					notify.MarkdownSanitize(name), currGB)
				if notifier.Telegram.SendMarkdown("流量预警", msg, "🚨") {
					sf.markNotified(instanceID, "overlimit")
				}
			}
		} else {
			log.Printf("[%s] 已停止止损 - %.2fGB", name, currGB)
			if sf.canNotify(instanceID, "overlimit", overlimitCooldown) {
				msg := fmt.Sprintf("机器: %s\n当前流量: %.2fGB\n状态: 流量超标，已保持关机 🛑",
					notify.MarkdownSanitize(name), currGB)
				if notifier.Telegram.SendMarkdown("流量超标提醒", msg, "🚨") {
					sf.markNotified(instanceID, "overlimit")
				}
			}
		}
	}
}

// tryStartInstance 尝试启动实例并轮询等待 Running。
func tryStartInstance(user *config.UserConfig, cfg *config.Config, client *aliyun.Client, sf *StateFile, notifier notify.Notifier, currGB float64, status string) {
	instanceID := user.InstanceID
	name := user.Name
	if name == "" {
		name = instanceID
	}
	st := sf.stateFor(instanceID)

	failures := st.StartFailures
	// 连续失败过多则降频重试（每 30 分钟一次）
	if failures >= maxStartFailures {
		if !st.LastRetry.IsZero() && time.Since(st.LastRetry) < resourceRetryCooldown {
			log.Printf("[%s] 已连续 %d 次启动失败，距下次重试还需 %s，本轮跳过",
				name, failures, (resourceRetryCooldown - time.Since(st.LastRetry)).Round(time.Second))
			return
		}
	}
	st.LastRetry = time.Now()
	log.Printf("[%s] 流量安全(%.2fGB)，尝试启动实例...", name, currGB)

	if err := client.StartInstance(instanceID); err != nil {
		st.StartFailures++
		log.Printf("[%s] StartInstance API 调用失败: %v，累计失败 %d 次", name, err, st.StartFailures)
		if sf.canNotify(instanceID, "start_failed", notifyCooldown) {
			msg := fmt.Sprintf("机器: %s\n当前流量: %.2fGB\n⚠️ 启动 API 调用失败: %v\n累计失败 %d 次，脚本将每 %d 分钟自动重试。",
				notify.MarkdownSanitize(name), currGB, err, st.StartFailures, int(resourceRetryCooldown/time.Minute))
			if notifier.Telegram.SendMarkdown("启动失败告警", msg, "🚨") {
				sf.markNotified(instanceID, "start_failed")
			}
		}
		return
	}
	log.Printf("[%s] StartInstance API 调用成功，等待实例进入 Running...", name)

	// 轮询等待 Running
	deadline := time.Now().Add(startWaitTimeout)
	started := false
	for time.Now().Before(deadline) {
		time.Sleep(startPollInterval)
		realStatus, err := client.GetInstanceStatus(instanceID)
		if err != nil {
			realStatus = "Unknown"
		}
		log.Printf("[%s] 等待启动... 当前状态: %s", name, realStatus)
		if realStatus == "Running" {
			started = true
			break
		}
		if realStatus == "Stopped" {
			log.Printf("[%s] 实例已回落到 Stopped 状态，启动被拒绝", name)
			break
		}
	}

	if started {
		st.StartFailures = 0
		st.LastRetry = time.Time{}
		log.Printf("[%s] 实例已恢复运行 ✅", name)
		if sf.canNotify(instanceID, "resumed", notifyCooldown) {
			msg := fmt.Sprintf("机器: %s\n当前流量: %.2fGB\n动作: 恢复运行 ✅",
				notify.MarkdownSanitize(name), currGB)
			if notifier.Telegram.SendMarkdown("恢复监控", msg, "✅") {
				sf.markNotified(instanceID, "resumed")
			}
		}
	} else {
		st.StartFailures++
		log.Printf("[%s] 启动超时或被拒绝，累计失败 %d 次", name, st.StartFailures)
		if sf.canNotify(instanceID, "start_failed", notifyCooldown) {
			msg := fmt.Sprintf("机器: %s\n当前流量: %.2fGB\n⚠️ 尝试启动但 %d 秒内未变为 Running 状态，累计失败 %d 次。\n脚本将每 %d 分钟自动重试，无需手动干预。",
				notify.MarkdownSanitize(name), currGB, int(startWaitTimeout/time.Second), st.StartFailures, int(resourceRetryCooldown/time.Minute))
			if notifier.Telegram.SendMarkdown("启动失败告警", msg, "🚨") {
				sf.markNotified(instanceID, "start_failed")
			}
		}
	}
}

// alertCheckFailure 连续多次巡检失败时提醒人工介入（"监控失明"）。
func alertCheckFailure(user *config.UserConfig, cfg *config.Config, sf *StateFile, notifier notify.Notifier, errText string) {
	instanceID := user.InstanceID
	name := user.Name
	if name == "" {
		name = instanceID
	}
	st := sf.stateFor(instanceID)
	if st.CheckFailures >= checkFailureThreshold && sf.canNotify(instanceID, "check_failed", notifyCooldown) {
		msg := fmt.Sprintf("机器: %s\n⚠️ 已连续 %d 次巡检失败，最近错误: %v\n期间流量监控与自动止损不可用，请人工确认实例状态。",
			notify.MarkdownSanitize(name), st.CheckFailures, errText)
		if notifier.Telegram.SendMarkdown("监控异常告警", msg, "🚨") {
			sf.markNotified(instanceID, "check_failed")
		}
	}
}
