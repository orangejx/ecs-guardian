// Package keeper 提供进程常驻调度器：每 5 分钟巡检 + 每日 09:00 日报，
// 并响应 SIGTERM/SIGINT 优雅退出。替代上游依赖 cron/systemd 的部分。
package keeper

import (
	"context"
	"log"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/orangejx/ecs-guardian/internal/monitor"
	"github.com/orangejx/ecs-guardian/internal/report"
)

const (
	monitorInterval = 5 * time.Minute
	reportHour      = 9
	reportMinute    = 0
)

// Config 常驻进程的调度配置。
type Config struct {
	// 是否同时启动日报调度
	EnableReport bool
}

// Run 以常驻方式运行 monitor 巡检（每 5 分钟）与日报（每天 09:00），直到收到 SIGTERM/SIGINT。
// monitor 单轮执行受内部超时保护；调度器自身失败时记录日志并继续下一轮。
func Run(cfg Config) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	var wg sync.WaitGroup
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// 巡检循环
	wg.Add(1)
	go func() {
		defer wg.Done()
		monitorLoop(runCtx)
	}()

	// 日报循环（可选）
	if cfg.EnableReport {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reportLoop(runCtx)
		}()
	}

	log.Printf("[keeper] 监控进程已启动：巡检每 %s，日报 %02d:%02d", monitorInterval, reportHour, reportMinute)

	<-ctx.Done()
	log.Printf("[keeper] 收到退出信号，正在停止...")
	cancel()
	// 给当前一轮巡检最多 5 分钟收尾（状态文件落盘）
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(monitorInterval):
		log.Printf("[keeper] 等待退出超时，强制退出")
	}
	return nil
}

// monitorLoop 每 monitorInterval 执行一轮巡检。
func monitorLoop(ctx context.Context) {
	// 启动即执行一轮，便于部署后立即验证配置
	runMonitorOnce()
	ticker := time.NewTicker(monitorInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runMonitorOnce()
		}
	}
}

func runMonitorOnce() {
	start := time.Now()
	if err := monitor.RunOnce(); err != nil {
		log.Printf("[monitor] 本轮巡检失败: %v", err)
	} else {
		log.Printf("[monitor] 本轮巡检完成，耗时 %s", time.Since(start).Round(time.Millisecond))
	}
}

// reportLoop 每天 reportHour:reportMinute 发送日报（本地时区，容器 TZ=Asia/Shanghai）。
func reportLoop(ctx context.Context) {
	for {
		next := nextReportTime()
		log.Printf("[report] 下次日报时间: %s", next.Local().Format("2006-01-02 15:04:05"))
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(next)):
		}
		// 到达 09:00，执行日报
		if err := report.SendDaily(); err != nil {
			log.Printf("[report] 日报发送失败: %v", err)
		}
	}
}

// nextReportTime 计算下一个 09:00（不含今天已过去的 09:00）。
func nextReportTime() time.Time {
	now := time.Now()
	next := time.Date(now.Year(), now.Month(), now.Day(), reportHour, reportMinute, 0, 0, now.Location())
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}
