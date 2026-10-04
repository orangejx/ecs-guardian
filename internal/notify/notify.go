package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/orangejx/ecs-guardian/internal/config"
)

// client 复用全局 HTTP 客户端（走系统代理环境变量）。
var client = &http.Client{Timeout: 15 * time.Second}

// MarkdownSanitize 去除 Telegram legacy Markdown 中的特殊字符，避免实例名/错误信息里的
// 特殊字符导致整条消息解析失败（与 Python 版 sanitize_markdown 一致）。
func MarkdownSanitize(text string) string {
	text = strings.TrimSpace(text)
	for _, ch := range []string{"_", "*", "`", "["} {
		text = strings.ReplaceAll(text, ch, " ")
	}
	return text
}

// RedactSensitive 隐藏查询串中的 AK/SK 与签名等敏感值（用于日志），保留主机名与错误原因。
func RedactSensitive(text string) string {
	out := text
	lower := strings.ToLower(text)
	for _, key := range []string{"accesskeyid", "accesskeysecret", "signature", "securitytoken", "password"} {
		for {
			idx := strings.Index(lower, key)
			if idx < 0 {
				break
			}
			// 向前找到 = 或 : 之后，向后找 & / 空白 等边界
			start := idx
			for start > 0 && out[start-1] != '=' && out[start-1] != ':' {
				start--
			}
			if start > 0 {
				start++
			}
			end := start
			for end < len(out) && !strings.ContainsRune("&,;\"'<>(){}[] \n\t", rune(out[end])) {
				end++
			}
			if end > start {
				out = out[:start] + "[REDACTED]" + out[end:]
				lower = strings.ToLower(out)
			} else {
				lower = lower[:idx] + "\x00" + lower[idx+len(key):]
			}
		}
	}
	return out
}

// TelegramAlert Telegram 告警通道。
type TelegramAlert struct {
	BotToken string
	ChatID   string
}

// Send 发送消息。parseMode 为 Markdown 时先尝试 Markdown，失败后回退纯文本（保证必达）。
func (t TelegramAlert) Send(text string) error {
	if strings.TrimSpace(t.BotToken) == "" || strings.TrimSpace(t.ChatID) == "" {
		return fmt.Errorf("Telegram 配置不完整")
	}
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", t.BotToken)
	payloads := []map[string]string{
		{"chat_id": t.ChatID, "text": text, "parse_mode": "Markdown"},
		{"chat_id": t.ChatID, "text": text},
	}
	var lastErr error
	for _, payload := range payloads {
		body, _ := json.Marshal(payload)
		resp, err := client.Post(url, "application/json", bytes.NewReader(body))
		if err != nil {
			lastErr = err
			continue
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return nil
		}
		lastErr = fmt.Errorf("HTTP %d: %s", resp.StatusCode, RedactSensitive(string(data)))
	}
	return lastErr
}

// SendMarkdown 以 Markdown 解析模式发送（带红act），失败自动降级纯文本。
func (t TelegramAlert) SendMarkdown(title, message, icon string) bool {
	text := fmt.Sprintf("%s *[%s]*\n\n%s", icon, RedactSensitive(title), RedactSensitive(message))
	err := t.Send(text)
	if err != nil {
		return false
	}
	return true
}

// BarkNotify Bark 推送通道。
type BarkNotify struct {
	BarkURL string
}

// Send 通过 GET 路径传参发送 Bark 推送（内容需 URL 编码），过长时截断。
func (b BarkNotify) Send(message string) error {
	base := strings.TrimRight(strings.TrimSpace(b.BarkURL), "/")
	if base == "" {
		return fmt.Errorf("Bark URL 未配置")
	}
	body := url.QueryEscape(truncateRunes(message, 1200))
	u := fmt.Sprintf("%s/Aliyun监控/%s", base, body)
	resp, err := client.Get(u)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Bark HTTP %d", resp.StatusCode)
	}
	return nil
}

// Notifier 统一通知入口。
type Notifier struct {
	Telegram TelegramAlert
	Bark     BarkNotify
}

// FromConfig 从全局配置构造 Notifier。
func FromConfig(cfg *config.Config) Notifier {
	return Notifier{
		Telegram: TelegramAlert{BotToken: cfg.Telegram.BotToken, ChatID: cfg.Telegram.ChatID},
		Bark:     BarkNotify{BarkURL: cfg.Bark.BarkURL},
	}
}

// splitMessage 按行边界切分超长消息（Telegram 单条上限约 4096 字符），
// 与 Python 版 split_message 行为一致。
func splitMessage(message string, limit int) []string {
	var chunks []string
	current := ""
	for _, line := range strings.Split(message, "\n") {
		for len(line) > limit {
			if current != "" {
				chunks = append(chunks, current)
				current = ""
			}
			chunks = append(chunks, line[:limit])
			line = line[limit:]
		}
		candidate := line
		if current != "" {
			candidate = current + "\n" + line
		}
		if len(candidate) > limit && current != "" {
			chunks = append(chunks, current)
			current = line
		} else {
			current = candidate
		}
	}
	if current != "" {
		chunks = append(chunks, current)
	}
	return chunks
}

// SendReport 发送日报：按行分片，每片先试 Markdown 再降级纯文本。
func (n Notifier) SendReport(message string) {
	const limit = 4000
	for i, chunk := range splitMessage(message, limit) {
		if err := n.Telegram.Send(chunk); err != nil {
			// 已自动降级纯文本，记录即可
		}
		_ = i
	}
	if err := n.Bark.Send(message); err != nil {
	}
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
