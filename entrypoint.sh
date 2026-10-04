#!/bin/sh
# ecs-guardian 容器入口：负责把环境变量渲染为 /data/config.json（首次启动或 FORCE_RECONFIG=1）。
# 配置/日志/状态全部落在 /data 卷，重新创建容器即自动恢复。
#
# 注意：本镜像的常驻进程是编译好的 Go 二进制，不依赖本脚本做进程管理；
# 本脚本仅负责"环境变量 → config.json"的初始化与校验。

set -eu

DATA_DIR="${ALIYUN_MONITOR_DATA:-/data}"
CONFIG_FILE="$DATA_DIR/config.json"
ALIYUN_MONITOR_BIN="${ALIYUN_MONITOR_BIN:-/usr/local/bin/ecs-guardian}"

log() { echo "[entrypoint] $*"; }

gen_config() {
    TG_TOKEN="${TELEGRAM_BOT_TOKEN:-}"
    TG_CHAT="${TELEGRAM_CHAT_ID:-}"
    ADMIN_IDS="${ADMIN_USERS:-}"
    BARK_URL="${BARK_URL:-}"

    if [ -z "$TG_TOKEN" ] || [ -z "$TG_CHAT" ]; then
        echo "[error] 未设置 TELEGRAM_BOT_TOKEN / TELEGRAM_CHAT_ID。请通过 docker run -e 传入。"
        exit 1
    fi

    # admin_users → JSON 数组（可含中英文逗号/空白）
    ADMIN_JSON="[]"
    if [ -n "$ADMIN_IDS" ]; then
        ADMIN_JSON=$(ALIYUN_ADMIN_IDS="$ADMIN_IDS" python3 - <<'PY'
import json, os, re, sys
raw = os.environ.get("ALIYUN_ADMIN_IDS", "").replace("，", ",")
ids = []
for item in re.split(r"[,\s]+", raw):
    if not item:
        continue
    try:
        ids.append(int(item))
    except ValueError:
        pass
ids = list(dict.fromkeys(ids))
if not ids:
    print("[]"); sys.exit(0)
print(json.dumps(ids, ensure_ascii=False))
PY
)
    fi

    # 解析 "name=..,ak=..,sk=..,region=..,instance_id=..,traffic_limit=.. [| ...]"
    parse_users() {
        [ -z "${ALIYUN_USERS:-}" ] && { echo "[]"; return; }
        USER_LIST=""
        OLD_IFS="$IFS"; IFS='|'
        for entry in $ALIYUN_USERS; do
            entry=$(echo "$entry" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//')
            [ -z "$entry" ] && continue
            NAME="" AK="" SK="" REGION="" INSTANCE="" LIMIT="180" RESGROUP="" BILL_EP="" CURRENCY=""
            OLD_IFS2="$IFS"; IFS=','
            for pair in $entry; do
                pair=$(echo "$pair" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//')
                key="${pair%%=*}"; value="${pair#*=}"
                case "$key" in
                    name) NAME="$value" ;;
                    ak) AK="$value" ;;
                    sk) SK="$value" ;;
                    region) REGION="$value" ;;
                    instance_id|instance|id) INSTANCE="$value" ;;
                    traffic_limit|limit) LIMIT="$value" ;;
                    resgroup) RESGROUP="$value" ;;
                    bill_endpoint|billing_endpoint) BILL_EP="$value" ;;
                    currency) CURRENCY="$value" ;;
                    *) ;;
                esac
            done
            IFS="$OLD_IFS2"
            if [ -z "$AK" ] || [ -z "$SK" ] || [ -z "$REGION" ] || [ -z "$INSTANCE" ]; then
                echo "[error] ALIYUN_USERS 条目字段缺失（需要 ak/sk/region/instance_id）: $entry" >&2
                exit 1
            fi
            if [ -z "$BILL_EP" ] && [ "$CURRENCY" = "$" ]; then
                BILL_EP="business.ap-southeast-1.aliyuncs.com"
            elif [ -z "$BILL_EP" ]; then
                BILL_EP="business.aliyuncs.com"
            fi
            [ -z "$CURRENCY" ] && CURRENCY="¥"
            case "$LIMIT" in
                ''|*[!0-9.]*) LIMIT="180" ;;
            esac
            JSON=$(python3 - "$NAME" "$AK" "$SK" "$REGION" "$INSTANCE" "$LIMIT" "$RESGROUP" "$BILL_EP" "$CURRENCY" <<'PY'
import json, sys
n,a,s,r,i,l,rg,be,cu = sys.argv[1:10]
print(json.dumps({
    "name": n, "ak": a, "sk": s, "region": r, "instance_id": i,
    "traffic_limit": float(l), "quota": 200, "bill_endpoint": be,
    "currency": cu, "resgroup": rg, "paused": False,
}, ensure_ascii=False))
PY
)
            if [ -n "$USER_LIST" ]; then USER_LIST="$USER_LIST,$JSON"; else USER_LIST="$JSON"; fi
        done
        IFS="$OLD_IFS"
        [ -n "$USER_LIST" ] && echo "[$USER_LIST]" || echo "[]"
    }
    USERS_JSON=$(parse_users)
    if [ "$USERS_JSON" = "[]" ]; then
        echo "[error] 未设置 ALIYUN_USERS（至少一个账号）。"
        exit 1
    fi

    cat > "$CONFIG_FILE" <<EOF
{
    "telegram": {
        "bot_token": "$TG_TOKEN",
        "chat_id": "$TG_CHAT"
    },
    "admin_users": $ADMIN_JSON,
    "bark": {
        "bark_url": "$BARK_URL"
    },
    "users": $USERS_JSON
}
EOF
    chmod 600 "$CONFIG_FILE"
    log "已写入 $CONFIG_FILE (admin_users=$ADMIN_JSON, users 数量=$(echo "$USERS_JSON" | grep -o 'instance_id' | wc -l | tr -d ' '))"
}

if [ -f "$CONFIG_FILE" ]; then
    if [ "${ALIYUN_MONITOR_FORCE_RECONFIG:-0}" = "1" ]; then
        log "检测到 ALIYUN_MONITOR_FORCE_RECONFIG=1，用环境变量重建配置（旧配置先备份）"
        cp "$CONFIG_FILE" "$CONFIG_FILE.bak.$(date +%Y%m%d%H%M%S)" 2>/dev/null || true
        gen_config
    else
        log "检测到已有配置 $CONFIG_FILE，沿用挂载配置。"
        log "提示：如需用环境变量重建，设置 ALIYUN_MONITOR_FORCE_RECONFIG=1 后重启容器。"
    fi
else
    gen_config
fi

chmod 600 "$CONFIG_FILE" 2>/dev/null || true

# 校验配置（Go 二进制自身也会校验，这里提前暴露明显错误）
if ! "$ALIYUN_MONITOR_BIN" validate >/dev/null 2>&1; then
    echo "[error] 配置校验失败，请检查 $CONFIG_FILE"
    "$ALIYUN_MONITOR_BIN" validate 2>&1 | head -5 || true
    exit 1
fi

log "配置校验通过，启动监控进程..."
exec "$ALIYUN_MONITOR_BIN" "$@"
