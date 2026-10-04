#!/bin/sh
# ecs-guardian 容器入口（位于容器根目录 /docker-entrypoint.sh，不放 /app）。
#
# 与 nginx 等官方镜像惯例一致：入口脚本放在镜像根路径，/app 只作为工作目录存放数据。
# 环境变量解析全部由 Go 二进制完成（优先级：环境变量 > config.json，读环境变量后会写入 config.json）。
# 本脚本只做启动前的最小准备：创建工作目录，然后 exec 二进制。

set -eu

DATA_DIR="${ALIYUN_MONITOR_DATA:-data}"
LOGS_DIR="${ALIYUN_MONITOR_LOGS:-logs}"

log() { echo "[entrypoint] $*"; }

# 数据目录必须可写（配置与状态文件落在这里）
if ! mkdir -p "$DATA_DIR" 2>/dev/null; then
    log "警告: 无法创建数据目录 $DATA_DIR，将只使用环境变量运行"
fi

# 日志目录可选：创建失败不阻塞（Go 内部会退回仅控制台输出）
mkdir -p "$LOGS_DIR" 2>/dev/null || log "提示: 无法创建日志目录 $LOGS_DIR，日志仅输出到控制台"

if [ -f "$DATA_DIR/config.json" ] && [ "${ALIYUN_MONITOR_FORCE_RECONFIG:-0}" != "1" ]; then
    log "检测到已有配置 $DATA_DIR/config.json（如需用环境变量重建，设置 ALIYUN_MONITOR_FORCE_RECONFIG=1）"
fi

exec /usr/local/bin/ecs-guardian "$@"
