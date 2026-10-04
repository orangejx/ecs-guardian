# ecs-guardian

阿里云 ECS **自动保活** 守护进程 —— 用 Go 实现，单静态二进制运行。为按量/抢占式 ECS 提供「流量止损 + 自动复活」，并通过 **Telegram** 推送告警与每日日报、远程控制开关机。

## 它做什么

- **自动复活**：每 5 分钟巡检一次。流量安全（未超过阈值）且实例处于 `Stopped` 时，自动调用 `StartInstance` 并轮询等待进入 `Running`。账号欠费被停机后，充上钱的下一个巡检周期内自动开机，无需手动干预。
- **流量止损**：当月 CDT 流量超过阈值时自动关机，防止超额度产生高额流量费；流量归零后的下个月自动恢复。
- **失败韧性**：启动失败累计计数，连续失败 3 次降频为每 30 分钟重试一次；连续 3 轮巡检失败发「监控失明」告警提醒人工介入。
- **Telegram 通知**：每日 09:00 日报（流量/账单/余额/实例状态）、流量预警、恢复/启动失败/监控异常告警，全部事件驱动并带冷却防刷屏。
- **Telegram 控制机器人**（可选）：管理员白名单，`/status`、`/start_instance`、`/stop`、`/reboot`、`/menu`、`/timers` 远程控制。
- **Bark 推送**（可选）：iOS 即时通知。

## 快速开始（Docker）

```bash
# 环境变量注入配置；数据（config/状态）与日志分两个卷持久化
docker run -d --name ecs-guardian --restart unless-stopped \
  -v /var/lib/ecs-guardian/data:/app/data \
  -v /var/lib/ecs-guardian/logs:/app/logs \
  -e TELEGRAM_BOT_TOKEN=123456:ABC \
  -e TELEGRAM_CHAT_ID=123456789 \
  -e 'ALIYUN_USERS=name=HK,ak=LTAI...,sk=...,region=cn-hongkong,instance_id=i-xxx,traffic_limit=180' \
  -e ADMIN_USERS='123456' \
  orangejx/ecs-guardian:latest
```

程序**优先使用环境变量**：只要设置了 `ALIYUN_USERS` 就用环境变量构建配置，并把结果写入 `<ALIYUN_MONITOR_DATA>/config.json` 持久化；未设置环境变量时回退读取该文件。重建容器后配置不丢失。

镜像内布局：可执行文件在 `/usr/local/bin/ecs-guardian`，工作目录 `/app`，其中 `/app/data` 存配置与状态、`/app/logs` 存日志。

### 环境变量

| 变量 | 必填 | 默认 | 说明 |
|---|---|---|---|
| `TELEGRAM_BOT_TOKEN` | 是 | — | @BotFather 创建机器人获得 |
| `TELEGRAM_CHAT_ID` | 是 | — | 接收通知的用户 ID（@userinfobot 可查） |
| `ALIYUN_USERS` | 是 | — | 被监控实例，多实例用 `\|` 分隔，见下 |
| `ADMIN_USERS` | 否 | — | 控制机器人管理员用户 ID（逗号分隔），配置后启动机器人 |
| `BARK_URL` | 否 | — | Bark 推送地址 |
| `ALIYUN_MONITOR_DATA` | 否 | `data` | 数据目录（config.json / 状态文件），容器内即 `/app/data` |
| `ALIYUN_MONITOR_LOGS` | 否 | `logs` | 日志目录，容器内即 `/app/logs`。日志可选，目录不可写时仅输出到控制台 |
| `ALIYUN_MONITOR_FORCE_RECONFIG` | 否 | `0` | `1`=强制用环境变量重建 config.json |

### ALIYUN_USERS 格式

每条目一个实例，`|` 分隔；同一 AK/SK 可复用在多个实例上：

```
name=HK,ak=LTAI...,sk=...,region=cn-hongkong,instance_id=i-xxx,traffic_limit=180 | name=SG,ak=...,sk=...,region=ap-southeast-1,instance_id=i-yyy,traffic_limit=100,currency=$
```

字段：`name`（备注，Telegram 命令按此区分）、`ak`、`sk`、`region`、`instance_id`（必填）、`traffic_limit`（GB，默认 180）、`resgroup`（RAM 资源组）、`bill_endpoint`、`currency`（`¥`/`$`，国际站账号填 `$`）。

> 注意：CDT 流量是**账号级**的。同一账号下多台实例共用流量池，超标时会一起停机；需要独立止损请拆分到不同账号。

### 阿里云 RAM 权限

建议使用 RAM 子账号并授予最小权限：
- `AliyunECSFullAccess`（开关机/查询）
- `AliyunCDTReadOnlyAccess`（查询流量，**缺少会报 `NoPermission`**）
- `AliyunBSSReadOnlyAccess`（账单/余额）

### 使用 docker compose

```bash
cp .env.example .env && vim .env
docker compose up -d          # 拉取镜像运行（发布版）
# 本地构建开发版：
# docker compose -f docker-compose.local.yml up -d
docker compose logs -f ecs-guardian
```

### 使用配置文件（config.json）

程序按以下优先级取配置：

1. **环境变量优先**：设置了 `ALIYUN_USERS` 就用环境变量构建配置，并写入 `<ALIYUN_MONITOR_DATA>/config.json`（默认 `data/config.json`，容器内 `/app/data/config.json`）。
2. **回退读文件**：未设置环境变量时，读取该 config.json。

也可以直接提供配置文件：把 [config.example.json](config.example.json) 复制为 `data/config.json` 并填真实值：

```bash
cp config.example.json data/config.json && vim data/config.json
docker compose -f docker-compose.local.yml up -d   # 用 docker compose 挂载 ./data 与 ./logs
```

> 配置字段与 `ALIYUN_USERS` 环境变量一一对应；完整字段说明见 [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)。

### 日志

日志为可选功能：默认同时输出到**控制台**（`docker logs` 可见）与**文件**。文件按天分级存放：

```
<ALIYUN_MONITOR_LOGS>/YYYYMM/DD.log          # 当天全部日志
<ALIYUN_MONITOR_LOGS>/YYYYMM/DD.error.log    # 当天 error 级日志（单独一份）
```

例如 `logs/202610/05.log`（2026 年 10 月 5 日）。默认日志目录为 `logs`（容器内 `/app/logs`）；目录不可写时程序照常运行，只是不落盘。

### 使用可执行文件（直接运行，不依赖 Docker）

从 GitHub **Release** 下载 `ecs-guardian-linux-amd64`（x86_64 服务器）或 `ecs-guardian-linux-arm64`（ARM 服务器），也可以本机用 `make` 或 `go build` 编译：

```bash
# 方式一：make（推荐，自动安装依赖并编译）
make deps && make build          # 编译当前平台，输出 bin/ecs-guardian
make build-all                   # 交叉编译 linux/amd64 + linux/arm64
make test                        # 编译 + go vet + 校验配置样例
make version                     # 编译并打印版本号
make run CONFIG=/path/to/config.json   # 编译后直接常驻运行

# 方式二：直接 go build
go build -o ecs-guardian .
```

> Makefile 的 `VERSION` 默认取最近 git tag（去 `v` 前缀），可覆盖：`make build VERSION=0.0.2`。

```bash
# 下载（替换成对应版本号）
curl -fsSL -o ecs-guardian \
  https://github.com/orangejx/ecs-guardian/releases/download/v0.0.2/ecs-guardian-linux-amd64
chmod +x ecs-guardian

# 快速启动：指定配置目录（config.json 放在该目录下）后直接运行，默认常驻
mkdir -p /etc/ecs-guardian
cp config.example.json /etc/ecs-guardian/config.json && vim /etc/ecs-guardian/config.json
ALIYUN_MONITOR_DATA=/etc/ecs-guardian ./ecs-guardian
```

配置路径有三种指定方式（优先级从高到低）：

```bash
# 1) 顶层 --config 直接指定配置文件（推荐，配置文件可在任意位置/任意命名）
./ecs-guardian --config /path/to/my-config.json

# 2) 子命令风格：--config 挂在子命令后
./ecs-guardian validate --config /path/to/my-config.json
./ecs-guardian monitor --once --config /path/to/my-config.json

# 3) 环境变量指定配置目录（config.json 固定放在该目录下）
ALIYUN_MONITOR_DATA=/etc/ecs-guardian ./ecs-guardian
```

> 说明：`--config` 只改变配置文件的读取位置；状态文件（`monitor_state.json` / `bot_state.json`）写入 `ALIYUN_MONITOR_DATA` 目录（默认 `data`），日志写入 `ALIYUN_MONITOR_LOGS` 目录（默认 `logs`）。请确保这两个目录可写。

常用子命令：

```bash
./ecs-guardian version                 # 查看版本
./ecs-guardian validate                # 仅校验配置后退出（排查配置问题用）
./ecs-guardian monitor --once          # 立即执行一轮巡检后退出（验证配置/鉴权）
./ecs-guardian report --now            # 立即发送一次日报
./ecs-guardian                         # 常驻：每5分钟巡检 + 每日09:00日报 + 控制机器人(可选)
./ecs-guardian --config my.json        # 常驻 + 显式指定配置文件
```

配合 systemd 长期运行（可选）：

```ini
# /etc/systemd/system/ecs-guardian.service
[Unit]
Description=ecs-guardian
After=network-online.target

[Service]
Type=simple
Environment=ALIYUN_MONITOR_DATA=/etc/ecs-guardian
ExecStart=/usr/local/bin/ecs-guardian
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
```

```bash
sudo cp ecs-guardian /usr/local/bin/
sudo systemctl daemon-reload && sudo systemctl enable --now ecs-guardian
journalctl -u ecs-guardian -f        # 查看日志
```

> 配置目录里程序会自动创建 `config.json` 之外的日志与状态文件（`*.log` / `monitor_state.json` / `bot_state.json`），请确保该目录可写。

## 手动构建（多架构）

```bash
docker buildx create --name multi --platform linux/amd64,linux/arm64 --use
docker buildx build --platform linux/amd64,linux/arm64 -t orangejx/ecs-guardian:latest --push .
```

## 发布（GitHub Actions）

推送 `v` 开头的 tag 触发自动发布：

```bash
git tag v1.0.0 && git push origin v1.0.0
```

自动完成：
1. 编译 `linux/amd64` + `linux/arm64` 静态可执行文件
2. 创建 GitHub **Release** 并上传二进制 + `SHA256SUMS`
3. 构建双架构 Docker 镜像并推送 **GHCR**（必发）与 **Docker Hub**（可选）

首次使用请配置仓库 Secrets：
- GHCR 用 `GITHUB_TOKEN` 自动鉴权，**无需配置**；
- 需要发 Docker Hub 时配置 `DOCKERHUB_USERNAME` 和 `DOCKERHUB_TOKEN`，不配则只发 GHCR。

> 把 `orangejx/ecs-guardian` 相关字样替换成你的 GitHub 用户名/仓库名后即可用。

## 致谢 / 来源

本项目源自 [10000ge10000/aliyun_monitor](https://github.com/10000ge10000/aliyun_monitor)（阿里云 CDT 流量监控 & 自动止损，Python 版）。ecs-guardian 用 Go 完整重写了它的保活逻辑与 Telegram 通知/控制能力，并修复了若干上游问题（HTTPS、AK/SK 空白、日报配额显示、定时任务时区等），但**核心思路与参数均继承自该上游项目**。在此向原作者致谢。

## Telegram 机器人命令

```
/start  检查机器人    /menu   交互菜单（按钮）
/list   实例列表      /status <实例名或ID>  查询状态
/start_instance <实例名或ID>  开机
/stop <实例名或ID>            关机（需确认）
/reboot <实例名或ID>          重启（需确认）
/timers  查看定时任务
```

## 工作原理（保活逻辑）

```
流量 < 阈值 且 Stopped → 自动 StartInstance → 轮询确认 Running
流量 < 阈值 且 Running → 不动
流量 ≥ 阈值            → StopInstance 止损
```

更完整的行为（启动失败降频重试、告警冷却、监控失明告警）见 [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)。

## 免责声明

本项目**会自动对你的阿里云 ECS 实例执行开机/关机操作**，并**自动查询你的账户余额与账单**。请务必在使用前理解其全部行为（见「工作原理」与 [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)），并自行承担使用风险：

- **不构成任何担保**：本项目按「原样」提供，不对任何直接、间接、偶然或后果性损失负责，包括但不限于：流量费用超支、账单异常、实例被误关机/误开机、数据丢失或业务中断。
- **请自行设置最后防线**：强烈建议同时在阿里云费用中心配置预算告警 / 垫底限额，作为本脚本之外的最后一道防线。
- **凭证安全由你负责**：配置中的 AK/SK 与 Bot Token 具有极高权限，请妥善保管（本仓库已在 `.gitignore` 中排除 `.env` 与 `data/`），并建议使用最小权限的 RAM 子账号。
- **上游免责声明延续**：本项目的逻辑继承自上游 [aliyun_monitor](https://github.com/10000ge10000/aliyun_monitor)，上游的免责条款同样适用——"作者不对因脚本异常、API 变更、依赖失效或配置错误导致的任何流量流失及费用负责"。

使用本项目即表示你理解并接受上述条款。

## License

**BSD 3-Clause** —— 本项目采用 [BSD 3-Clause 许可证](./LICENSE) 开源。

你可以在保留版权声明的前提下自由使用、修改、再分发（含闭源与商业用途）。**BSD 3-Clause 要求任何再分发（无论是源码还是二进制形式）都必须保留本项目的版权声明与许可条款，即必须标注本项目来源。**

> 致谢：本项目源自 [10000ge10000/aliyun_monitor](https://github.com/10000ge10000/aliyun_monitor)，见上文「致谢 / 来源」。
