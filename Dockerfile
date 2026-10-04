# ecs-guardian —— 阿里云 ECS 自动保活 & Telegram 通知守护进程
#
# 用 Go 实现、单静态二进制运行。为按量/抢占式 ECS 提供"流量止损 + 自动复活"，
# 并通过 Telegram 机器人推送告警/日报、远程控制开关机。
#
# 特性：
#   - 每 5 分钟巡检：流量安全时自动拉起 Stopped 实例；流量超标时自动关机止损
#   - 启动失败降频重试（30 分钟冷却），连续巡检失败发"监控失明"告警
#   - 每日 09:00 发送 Telegram 日报（流量/账单/余额/实例状态）
#   - 可选 Telegram 控制机器人：管理员白名单、远程开关机/重启、定时任务
#   - 环境变量注入配置，工作目录 /app 下 data/ 卷持久化（config.json / 状态 / 日志）
#
# 构建（本机）：
#   docker build -t orangejx/ecs-guardian:latest .
# 构建多架构并推送（Linux x86_64 + arm64）：
#   docker buildx build --platform linux/amd64,linux/arm64 \
#       -t orangejx/ecs-guardian:latest --push .
# （GitHub Actions 会在打 tag 时自动构建并发布到 Docker Hub 与 GHCR，见 .github/workflows/release.yml）
#
# 运行：
#   docker run -d --name ecs-guardian --restart unless-stopped \
#     -v /var/lib/ecs-guardian:/app/data \
#     -e TELEGRAM_BOT_TOKEN=123456:ABC \
#     -e TELEGRAM_CHAT_ID=123456789 \
#     -e 'ALIYUN_USERS=name=HK,ak=LTAI...,sk=...,region=cn-hongkong,instance_id=i-xxx,traffic_limit=180' \
#     -e ADMIN_USERS='123456' \
#     orangejx/ecs-guardian:latest
#
# 环境变量：
#   TELEGRAM_BOT_TOKEN / TELEGRAM_CHAT_ID    必填
#   ALIYUN_USERS                             必填，多实例用 | 分隔
#   ADMIN_USERS                              选填，Telegram 用户 ID（逗号分隔），配置后启动控制机器人
#   BARK_URL                                 选填，Bark 推送地址
#   ALIYUN_MONITOR_DATA                     选填，数据目录（config.json/状态），默认 data（即 /app/data）
#   ALIYUN_MONITOR_LOGS                     选填，日志目录，默认 <data>/logs（/app/data/logs）
#   ALIYUN_MONITOR_FORCE_RECONFIG           选填，1=用环境变量重建 config.json（默认沿用挂载配置）
#
# 配置持久化：config.json / monitor_state.json / bot_state.json 在 /app/data；日志在 /app/data/logs。

# ---- 构建阶段：编译静态二进制 ----
FROM golang:1.26-alpine AS builder

WORKDIR /src
# 先拷贝 go.mod / go.sum 以利用层缓存
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# CGO_ENABLED=0 静态编译，避免 glibc 依赖，兼容 alpine 与跨架构
ARG TARGETOS TARGETARCH
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/ecs-guardian .

# ---- 运行阶段：纯 alpine ----
FROM alpine:3.20

# 时区数据 + ca-certificates（Go 静态二进制默认使用系统 CA 池）
# curl 仅用于容器内手工排障
RUN apk add --no-cache tzdata ca-certificates curl

# 可执行文件放 /usr/local/bin（系统路径，FHS 惯例）
COPY --from=builder /out/ecs-guardian /usr/local/bin/ecs-guardian
# 入口脚本放容器根目录（惯例，不放 /app）
COPY docker-entrypoint.sh /docker-entrypoint.sh
RUN chmod +x /docker-entrypoint.sh

ENV TZ=Asia/Shanghai \
    ALIYUN_MONITOR_DATA=data \
    ALIYUN_MONITOR_LOGS=logs

# 数据与日志是两个独立持久化卷：数据（config/状态）挂 /app/data，日志挂 /app/logs
VOLUME ["/app/data", "/app/logs"]
WORKDIR /app

# 默认常驻运行：每5分钟巡检 + 每日09:00日报 + 控制机器人(若配置了 ADMIN_USERS)
ENTRYPOINT ["/docker-entrypoint.sh"]

# 其他用法：
#   docker run ... ecs-guardian monitor --once   # 立即执行一轮巡检
#   docker run ... ecs-guardian report --now     # 立即发送日报
