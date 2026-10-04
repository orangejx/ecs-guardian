// Package main 是 ecs-guardian 的入口。
//
// 用法：
//
//	ecs-guardian                                     # 常驻：每5分钟巡检 + 每日09:00日报 + 控制机器人(可选)
//	ecs-guardian --config /path/to/config.json       # 常驻，显式指定配置文件
//	ecs-guardian monitor --once                      # 立即执行一轮巡检后退出
//	ecs-guardian report --now                        # 立即发送日报后退出
//	ecs-guardian validate                            # 仅校验配置后退出
//	ecs-guardian version                             # 打印版本号
//
// 配置路径优先级：--config 显式指定 > ALIYUN_MONITOR_DATA/config.json（默认 /data/config.json）。
// 日志与状态文件（*.log / monitor_state.json / bot_state.json）始终写入 ALIYUN_MONITOR_DATA 目录。
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

// version 由构建工具注入（GitHub Actions: -X main.version={{.version}}）。
var version = "dev"

// configFlag 把 --config 挂到指定 FlagSet 上，并在解析后设置 config.ConfigOverride。
func configFlag(fs *flag.FlagSet) *string {
	cfgPath := fs.String("config", "", "config.json 的路径（默认 ALIYUN_MONITOR_DATA/config.json）")
	return cfgPath
}

func applyConfigPath(p *string) {
	if p != nil && *p != "" {
		config.ConfigOverride = *p
	}
}

func main() {
	// 顶层 --config 形式：ecs-guardian --config <path> [validate|monitor|report|version]
	if len(os.Args) >= 2 && (os.Args[1] == "--config" || os.Args[1] == "-config") {
		if len(os.Args) < 3 {
			log.Fatal("--config 需要一个文件路径参数")
		}
		config.ConfigOverride = os.Args[2]
		sub := "server"
		if len(os.Args) > 3 {
			sub = os.Args[3]
		}
		switch sub {
		case "validate":
			runValidateCmd()
		case "monitor":
			runMonitorOnceCmd()
		case "report":
			runReportNowCmd()
		case "version":
			fmt.Printf("ecs-guardian %s\n", version)
		default:
			runServer()
		}
		return
	}

	if len(os.Args) < 2 {
		runServer()
		return
	}

	switch os.Args[1] {
	case "monitor":
		runMonitorOnceCmd()
	case "report":
		runReportNowCmd()
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
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	cfgPath := configFlag(fs)
	fs.Parse(os.Args[2:])
	applyConfigPath(cfgPath)

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
func runMonitorOnceCmd() {
	fs := flag.NewFlagSet("monitor", flag.ExitOnError)
	once := fs.Bool("once", false, "立即执行一轮巡检后退出")
	cfgPath := configFlag(fs)
	fs.Parse(os.Args[2:])
	applyConfigPath(cfgPath)

	if !*once {
		// 与常驻行为一致：未显式 --once 时也执行一轮（历史兼容）
	}
	if err := monitor.RunOnce(); err != nil {
		log.Fatalf("[monitor] 巡检失败: %v", err)
	}
}

// runReportNowCmd 立即发送日报后退出。
func runReportNowCmd() {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	now := fs.Bool("now", false, "立即发送日报")
	cfgPath := configFlag(fs)
	fs.Parse(os.Args[2:])
	applyConfigPath(cfgPath)

	if !*now {
		// 未传 --now 也直接发送（历史兼容）
	}
	if err := report.SendDaily(); err != nil {
		log.Fatalf("[report] 日报发送失败: %v", err)
	}
}
