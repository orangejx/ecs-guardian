package report

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/orangejx/ecs-guardian/internal/aliyun"
	"github.com/orangejx/ecs-guardian/internal/config"
	"github.com/orangejx/ecs-guardian/internal/notify"
)

// SendDaily 生成并发送每日日报（等价于 Python report.py）。
func SendDaily() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	notifier := notify.FromConfig(cfg)

	var lines []string
	lines = append(lines, "📊 *[阿里云多账号 - 每日财报]*")
	lines = append(lines, fmt.Sprintf("📅 日期: %s\n", time.Now().Format("2006-01-02")))

	// 同一账号(AK)的余额只查询一次
	balanceCache := map[string][2]any{}

	for i := range cfg.Users {
		user := &cfg.Users[i]
		userName := strings.TrimSpace(user.Name)
		if userName == "" {
			userName = user.InstanceID
		}
		if userName == "" {
			userName = "Unknown_Device"
		}

		if user.Paused || user.Disabled {
			log.Printf("[%s] 监控已暂停，日报仅标注暂停状态", userName)
			lines = append(lines,
				fmt.Sprintf("👤 *%s* (暂停)\n   ⏸️ 监控: 已暂停\n",
					notify.MarkdownSanitize(userName)))
			continue
		}

		client, err := aliyun.NewClient(user.AK, user.SK, user.Region)
		if err != nil {
			log.Printf("[%s] 创建客户端失败: %v", userName, err)
			lines = append(lines, fmt.Sprintf("❌ *%s* Error: 创建客户端失败\n",
				notify.MarkdownSanitize(userName)))
			continue
		}

		// 1. CDT 流量
		trafficGB := -1.0
		trafficBytes, err := client.GetCDTInternetTraffic(aliyun.CDTQueryParams{})
		if err == nil {
			trafficGB = float64(trafficBytes) / (1024 * 1024 * 1024)
		} else {
			log.Printf("[%s] 流量查询失败: %v", userName, err)
		}

		// 2. 账单（优先实例级，失败回退总览）
		billingCycle := time.Now().Format("2006-01")
		billAmount := -1.0
		billCurrency := "USD"
		if amount, currency, err := client.QueryInstanceBill(user.InstanceID, billingCycle); err == nil {
			billAmount = amount
			billCurrency = currency
		} else {
			// 回退：QueryBillOverview
			ep := user.BillEndpoint
			if ep == "" {
				ep = "business.ap-southeast-1.aliyuncs.com"
			}
			if amount, currency, err2 := client.QueryBillOverview(ep, billingCycle); err2 == nil {
				billAmount = amount
				billCurrency = currency
			}
		}

		// 3. 余额（同 AK 复用缓存）
		balanceAmount := -1.0
		balanceCurrency := ""
		if v, ok := balanceCache[user.AK]; ok {
			balanceAmount = v[0].(float64)
			balanceCurrency = v[1].(string)
		} else if amount, currency, err := client.QueryAccountBalance(user.BillEndpoint); err == nil {
			balanceAmount = amount
			balanceCurrency = currency
			balanceCache[user.AK] = [2]any{amount, currency}
		} else {
			log.Printf("[%s] 余额查询失败: %v", userName, err)
		}

		// 4. ECS 状态/IP/规格
		status, ip, spec := "NotFound", "N/A", "N/A"
		if detail, err := client.GetInstanceDetail(user.InstanceID); err == nil {
			status = detail.Status
			ip = detail.IP
			if ip == "" {
				ip = "无公网IP"
			}
			mem := float64(detail.MemoryMB) / 1024
			if detail.MemoryMB > 0 && detail.MemoryMB%1024 == 0 {
				spec = fmt.Sprintf("%dC%dG", detail.CPU, int(mem))
			} else {
				spec = fmt.Sprintf("%dC%.1fG", detail.CPU, mem)
			}
		}

		// 5. 判定
		quota := user.Quota
		if quota <= 0 {
			quota = user.TrafficLimit
		}
		if quota <= 0 {
			quota = 200
		}
		percent := 0.0
		trafficStr := "⚠️ 查询失败"
		if trafficGB >= 0 {
			percent = trafficGB / quota * 100
			trafficStr = fmt.Sprintf("%.2f GB (%.1f%%)", trafficGB, percent)
		}

		billStr := "Fail"
		if billAmount != -1 {
			if billCurrency == "CNY" {
				billStr = fmt.Sprintf("¥%.2f", billAmount)
			} else {
				sym := user.Currency
				if sym == "" {
					sym = "$"
				}
				billStr = fmt.Sprintf("%s%.2f", sym, billAmount)
			}
		}

		balanceStr := "⚠️ 查询失败"
		if balanceAmount != -1 {
			sym := map[string]string{"CNY": "¥", "USD": "$"}[balanceCurrency]
			if sym == "" {
				sym = user.Currency
			}
			if sym == "" {
				sym = "$"
			}
			balanceStr = fmt.Sprintf("%s%.2f", sym, balanceAmount)
			if balanceAmount < 0 {
				balanceStr += " ⚠️ 欠费"
			}
		}

		statusIcon := "✅"
		if billAmount == -1 {
			statusIcon = "⚠️ 账单查询异常"
		}
		if trafficGB >= 0 && trafficGB > user.TrafficLimit {
			statusIcon = "⚠️ 流量超标"
		}
		if trafficGB < 0 {
			statusIcon = "⚠️ 流量查询异常"
		}

		runIcon := "🟢"
		switch status {
		case "Running":
			runIcon = "🟢"
		case "Stopped":
			runIcon = "⚫"
		case "NotFound":
			runIcon = "❓"
		default:
			runIcon = "🔴"
		}

		lines = append(lines, fmt.Sprintf(
			"👤 *%s* (%s)\n   🖥️ 状态: %s %s\n   🌐 IP: `%s`\n   📉 流量: %s\n   💰 账单: *%s*\n   💳 余额: *%s*\n   📝 评价: %s\n",
			notify.MarkdownSanitize(userName), spec,
			runIcon, status,
			ip,
			trafficStr,
			billStr,
			balanceStr,
			statusIcon,
		))
	}

	notifier.SendReport(strings.Join(lines, "\n"))
	return nil
}
