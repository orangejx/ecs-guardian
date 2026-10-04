# ecs-guardian 架构说明

## 总体结构

```
main.go                   入口：子命令分发（常驻 / monitor --once / report --now / validate / version）
internal/
  aliyun/                 阿里云 API 客户端封装
    aliyun.go             ECS/BSS 服务包客户端 + CDT 的 RPC-2 签名直连
  config/                 配置加载/校验/持久化（默认 data/config.json；环境变量优先）
  keeper/                 常驻调度器：每 5 分钟巡检 + 每日 09:00 日报
  monitor/                巡检与保活决策（核心）
  report/                 每日日报生成与发送
  bot/                    Telegram 控制机器人
  notify/                 Telegram / Bark 通知封装
```

## 保活决策（internal/monitor）

每 5 分钟对每个实例执行：

```
1. 查 CDT 当月流量 → currGB
2. 查实例状态 → status
3. 决策：
   流量 < limit 且 Stopped   → StartInstance + 轮询等待 Running（180s 超时）
   流量 < limit 且 Running   → 不动
   流量 < limit 且 中间态     → 不动
   流量 ≥ limit 且 Running   → StopInstance（止损）
   流量 ≥ limit 且 Stopped   → 保持关机，每天提醒一次
```

### 失败与降频

- 启动失败 → `start_failures +1`；≥3 次后改为每 30 分钟重试一次（`resourceRetryCooldown`），期间发「启动失败告警」。
- 启动成功 → 计数清零，发「恢复监控」。
- 流量/状态任一查询失败 → `check_failures +1`；连续 ≥3 次发「监控失明」告警（`check_failed`），期间自动止损不可用，需人工介入。
- 通知冷却：通用事件 1 小时、流量超标提醒 24 小时，防刷屏。
- 单实例巡检硬超时 300s（SIGALRM 等价物），防止单个请求永久阻塞整轮。

### 状态持久化

`monitor_state.json`：每实例的 `start_failures` / `last_retry_ts` / `check_failures` / `last_notify`。
崩溃或重启后从该文件恢复冷却与计数，避免重启即重复告警。

## CDT 流量说明

- 接口：`cdt.aliyuncs.com`，RPC-2，`ListCdtInternetTraffic`，Version `2021-08-13`。
- 官方 Go SDK 没有生成 CDT 服务包，`internal/aliyun` 用 `requests.NewCommonRequest` + SDK 内置 RPC-2 签名直接调用。
- **流量是账号级月度总量**，不是单实例。同账号多实例共用流量池。
- 需要 RAM 权限 `AliyunCDTReadOnlyAccess`（缺省报 `NoPermission: cdt:ListCdtInternetTraffic`）。

## 网络与超时

- 所有阿里云请求显式 HTTPS（官方 Go SDK 默认 HTTP，用 `WithScheme("HTTPS")` 覆盖）。
- 连接 5s / 读取 15s 超时；Telegram 客户端自定义 transport，总超时 90s，走系统代理环境变量。
- Telegram 长轮询 `getUpdates` 在网络中断时（`unexpected EOF`）由库自动重连（3s 退避），并带请求/响应日志便于排查。

## 进程模型

单二进制常驻，无 cron/systemd 依赖：
- 启动即执行一轮巡检（部署后立即可验证配置）。
- 巡检每 5 分钟；日报每日 09:00（容器时区 `TZ=Asia/Shanghai`）。
- `SIGTERM`/`SIGINT` 优雅退出，状态文件先落盘再退。

## 配置与日志目录

数据与日志相互独立（环境变量可覆盖）：

| 目录 | 环境变量 | 默认值 | 容器内路径 |
|---|---|---|---|
| 数据（config/状态） | `ALIYUN_MONITOR_DATA` | `data` | `/app/data` |
| 日志 | `ALIYUN_MONITOR_LOGS` | `logs` | `/app/logs` |

- `config.json`：优先由环境变量生成并写入（`ALIYUN_MONITOR_FORCE_RECONFIG=1` 强制重建）；无环境变量时读取该文件。
- 日志按天分级：`<logs>/YYYYMM/DD.log` 与 `DD.error.log`；目录不可写时仅输出到控制台。
- `<data>/monitor_state.json`：巡检状态（失败计数/通知冷却）。
- `<data>/bot_state.json`：机器人定时任务。
