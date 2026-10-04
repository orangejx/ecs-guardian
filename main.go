// Package main 是 ecs-guardian 的入口。
//
// 用法：
//
//	ecs-guardian                   # 常驻：每5分钟巡检 + 每日09:00日报 + 控制机器人(可选)
//	ecs-guardian monitor --once    # 立即执行一轮巡检后退出
//	ecs-guardian report --now      # 立即发送日报后退出
//	ecs-guardian validate          # 仅校验配置后退出
//	ecs-guardian version           # 打印版本号
//
// 配置：ALIYUN_MONITOR_DATA（默认 /data）目录下的 config.json。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/orangejx/ecs-guardian/internal/bot"
	"github.com/orangejx/ecs-guardian/internal/config"
	"github.com/orangejx/ecs-guardian/internal/keeper"
	"github.com/orangejx/ecs-guardian/internal/monitor"
	"github.com/orangejx/ecs-guardian/internal/report"
)

// version 由构建工具注入（goreleaser: -X main.version={{.Version}}）。
var version = "dev"

func main() {
	// 顶层 flag 解析：支持子命令风格
	if len(os.Args) < 2 {
		runServer()
		return
	}
	switch os.Args[1] {
	case "monitor":
		runMonitorOnceCmd(os.Args[2:])
	case "report":
		runReportNowCmd(os.Args[2:])
	case "validate":
		runValidateCmd()
	case "version":
		fmt.Printf("ecs-guardian %s\n", version)
	default:
		runServer()
	}
}

// runValidateCmd 仅校验配置后退出（供容器 entrypoint 调用）。
func runValidateCmd() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("配置校验失败: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		log.Fatalf("配置校验失败: %v", err)
	}
	fmt.Println("配置校验通过")
}

// runServer 常驻模式：巡检 + 日报 + 机器人。
func runServer() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.Printf("[main] 阿里云 CDT 流量监控 & 自动止损 (Go 版) 启动")

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("[main] 加载配置失败: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		log.Fatalf("[main] 配置校验失败: %v", err)
	}

	// 控制机器人（可选）：配置了 admin_users 才启动
	var botCancel context.CancelFunc
	if len(cfg.AdminUsers) > 0 {
		botInst, err := bot.New(cfg)
		if err != nil {
			log.Printf("[main] 控制机器人未启动: %v", err)
		} else {
			ctx, cancel := context.WithCancel(context.Background())
			botCancel = cancel
			go func() {
				if err := botInst.Run(ctx); err != nil {
					log.Printf("[main] 控制机器人退出: %v", err)
				}
			}()
		}
	}
	defer func() {
		if botCancel != nil {
			botCancel()
		}
	}()

	if err := keeper.Run(keeper.Config{EnableReport: true}); err != nil {
		log.Fatalf("[main] 常驻进程退出: %v", err)
	}
}

// runMonitorOnceCmd 执行一轮巡检后退出。
func runMonitorOnceCmd(args []string) {
	fs := flag.NewFlagSet("monitor", flag.ExitOnError)
	once := fs.Bool("once", false, "立即执行一轮巡检后退出")
	fs.Parse(args)
	_ = once
	if err := monitor.RunOnce(); err != nil {
		log.Fatalf("[monitor] 巡检失败: %v", err)
	}
}

// runReportNowCmd 立即发送日报后退出。
func runReportNowCmd(args []string) {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	now := fs.Bool("now", false, "立即发送日报")
	fs.Parse(args)
	_ = now
	if err := report.SendDaily(); err != nil {
		log.Fatalf("[report] 日报发送失败: %v", err)
	}
}
