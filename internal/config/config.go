package config

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// UserConfig 描述一个被监控的阿里云账号/实例。
// 字段与上游 install.sh 生成的 config.json 完全一致。
type UserConfig struct {
	Name         string  `json:"name"`
	AK           string  `json:"ak"`
	SK           string  `json:"sk"`
	Region       string  `json:"region"`
	InstanceID   string  `json:"instance_id"`
	TrafficLimit float64 `json:"traffic_limit"`
	Quota        float64 `json:"quota"`
	BillEndpoint string  `json:"bill_endpoint"`
	Currency     string  `json:"currency"`
	ResGroup     string  `json:"resgroup"`
	Paused       bool    `json:"paused"`
	Disabled     bool    `json:"disabled,omitempty"`

	// 扩展字段（可选）
	BillThreshold float64 `json:"bill_threshold,omitempty"`
	UsdCNYRate    float64 `json:"usd_cny_rate,omitempty"`
}

// TelegramConfig Telegram 通知配置。
type TelegramConfig struct {
	BotToken string `json:"bot_token"`
	ChatID   string `json:"chat_id"`
}

// BarkConfig Bark 推送配置。
type BarkConfig struct {
	BarkURL string `json:"bark_url"`
}

// Config 是整个监控程序的配置，对应 /data/config.json。
type Config struct {
	Telegram   TelegramConfig `json:"telegram"`
	AdminUsers []int64        `json:"admin_users"`
	Bark       BarkConfig     `json:"bark"`
	Users      []UserConfig   `json:"users"`
}

// ConfigOverride 是全局配置路径覆盖（由 main 解析 --config 设置）。
// 优先级：--config 显式指定 > ALIYUN_MONITOR_DATA/config.json（默认 /data/config.json）。
var ConfigOverride string

// EnvPrecedence 控制配置来源优先级（由 main 设置，默认 true）：
//
//	true  = 优先解析环境变量；若 ALIYUN_USERS 存在则用环境变量配置，否则回退读取 config.json
//	false = 始终读取 config.json（忽略环境变量）
var EnvPrecedence = true

// DefaultDataDir 返回数据目录（环境变量 ALIYUN_MONITOR_DATA，默认 "data"）。
// 默认 "data"（当前工作目录下的 data 目录），容器内即 /app/data。
func DefaultDataDir() string {
	if d := os.Getenv("ALIYUN_MONITOR_DATA"); d != "" {
		return d
	}
	return "data"
}

// DefaultLogsDir 返回日志目录（环境变量 ALIYUN_MONITOR_LOGS，默认 "logs"）。
// 数据与日志是两回事：日志默认独立于数据目录（容器内即 /app/logs），
// 除非用户显式把它们设到同一处。
func DefaultLogsDir() string {
	if d := os.Getenv("ALIYUN_MONITOR_LOGS"); d != "" {
		return d
	}
	return "logs"
}

// ConfigPath 返回 config.json 的路径。设置了 --config 时优先使用该路径。
func ConfigPath() string {
	if ConfigOverride != "" {
		return ConfigOverride
	}
	return filepath.Join(DefaultDataDir(), "config.json")
}

// Load 加载配置。优先级：
//  1. 环境变量（TELEGRAM_BOT_TOKEN / TELEGRAM_CHAT_ID / ALIYUN_USERS / ADMIN_USERS / BARK_URL），
//     只要 ALIYUN_USERS 非空就优先使用，并尝试写入 config.json（持久化）。
//     写入失败（如目录只读）只告警，不中断——环境变量配置照样生效。
//  2. 否则读取 config.json（--config 或 ALIYUN_MONITOR_DATA/config.json）。
func Load() (*Config, error) {
	if EnvPrecedence {
		if cfg, ok, err := LoadFromEnv(); err != nil {
			return nil, err
		} else if ok {
			// 环境变量提供了完整配置：尝试写入配置文件以便下次快速启动 / 便于排查。
			// 写入失败不致命（例如 data 目录只读），仅记录日志。
			if err := cfg.Save(); err != nil {
				log.Printf("[config] 写入配置文件失败（不影响运行）: %v", err)
			}
			return cfg, nil
		}
	}
	return LoadFromFile()
}

// LoadFromEnv 从环境变量解析配置。返回 ok=false 表示环境变量不完整（未设置 ALIYUN_USERS），
// 此时应回退到读取配置文件。
func LoadFromEnv() (*Config, bool, error) {
	raw := strings.TrimSpace(os.Getenv("ALIYUN_USERS"))
	if raw == "" {
		return nil, false, nil
	}
	// 剥离两端成对的单/双引号（部分注入方式会把 .env / compose 的引号原样传入）
	raw = stripWrappingQuotes(raw)

	users, err := ParseUsers(raw)
	if err != nil {
		return nil, true, err
	}

	cfg := &Config{
		Telegram: TelegramConfig{
			BotToken: strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN")),
			ChatID:   strings.TrimSpace(os.Getenv("TELEGRAM_CHAT_ID")),
		},
		AdminUsers: parseAdminUsers(os.Getenv("ADMIN_USERS")),
		Bark:       BarkConfig{BarkURL: strings.TrimSpace(os.Getenv("BARK_URL"))},
		Users:      users,
	}
	return cfg, true, nil
}

// stripWrappingQuotes 去掉字符串两端成对的单/双引号。
func stripWrappingQuotes(s string) string {
	if len(s) >= 2 {
		if (s[0] == '\'' && s[len(s)-1] == '\'') || (s[0] == '"' && s[len(s)-1] == '"') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// parseAdminUsers 解析 ADMIN_USERS（支持中英文逗号/空白分隔），返回去重后的数字 ID 列表。
func parseAdminUsers(raw string) []int64 {
	raw = strings.NewReplacer("，", ",", " ", ",", "\t", ",", "\n", ",").Replace(strings.TrimSpace(raw))
	if raw == "" {
		return nil
	}
	seen := map[int64]bool{}
	var ids []int64
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if id, err := strconv.ParseInt(part, 10, 64); err == nil && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids
}

// ParseUsers 解析 ALIYUN_USERS。格式：
//
//	name=备注,ak=..,sk=..,region=..,instance_id=..,traffic_limit=180,resgroup=..,bill_endpoint=..,currency=¥|$ | ...
//
// 条目用 | 分隔，字段用 , 分隔。ak/sk/region/instance_id 必填；currency=$ 且未指定
// bill_endpoint 时默认国际站账单节点。
func ParseUsers(raw string) ([]UserConfig, error) {
	var users []UserConfig
	for _, entry := range strings.Split(raw, "|") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		// 逐条剥离两端成对引号（防御单个条目前后残留引号）
		entry = stripWrappingQuotes(entry)

		fields := map[string]string{}
		for _, pair := range strings.Split(entry, ",") {
			pair = strings.TrimSpace(pair)
			if pair == "" {
				continue
			}
			key, value, ok := strings.Cut(pair, "=")
			if !ok {
				continue
			}
			fields[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}

		ak := fields["ak"]
		sk := fields["sk"]
		region := fields["region"]
		instanceID := fields["instance_id"]
		if instanceID == "" {
			instanceID = fields["instance"]
		}
		if instanceID == "" {
			instanceID = fields["id"]
		}
		if ak == "" || sk == "" || region == "" || instanceID == "" {
			return nil, fmt.Errorf("ALIYUN_USERS 条目字段缺失（需要 ak/sk/region/instance_id）: %s", entry)
		}

		currency := fields["currency"]
		billEndpoint := fields["bill_endpoint"]
		if billEndpoint == "" {
			billEndpoint = fields["billing_endpoint"]
		}
		if billEndpoint == "" && currency == "$" {
			billEndpoint = "business.ap-southeast-1.aliyuncs.com"
		}
		if billEndpoint == "" {
			billEndpoint = "business.aliyuncs.com"
		}
		if currency == "" {
			currency = "¥"
		}

		limit := 180.0
		if v := fields["traffic_limit"]; v != "" {
			if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
				limit = f
			}
		} else if v := fields["limit"]; v != "" {
			if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
				limit = f
			}
		}

		users = append(users, UserConfig{
			Name:         fields["name"],
			AK:           ak,
			SK:           sk,
			Region:       region,
			InstanceID:   instanceID,
			TrafficLimit: limit,
			Quota:        200,
			BillEndpoint: billEndpoint,
			Currency:     currency,
			ResGroup:     fields["resgroup"],
			Paused:       false,
		})
	}
	if len(users) == 0 {
		return nil, fmt.Errorf("ALIYUN_USERS 未包含有效条目")
	}
	return users, nil
}

// LoadFromFile 从磁盘加载配置（--config 或 ALIYUN_MONITOR_DATA/config.json）。
func LoadFromFile() (*Config, error) {
	path := ConfigPath()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败 (%s): %w", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件失败 (%s): %w", path, err)
	}
	if len(cfg.Users) == 0 {
		return nil, fmt.Errorf("配置文件中没有监控实例 (users 为空)")
	}
	return &cfg, nil
}

// Save 原子写入配置（先写临时文件再 rename，避免进程被杀写坏 config.json）。
func (c *Config) Save() error {
	path := ConfigPath()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "    ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Validate 检查配置基本完整性，返回错误信息（不含敏感字段值）。
func (c *Config) Validate() error {
	if strings.TrimSpace(c.Telegram.BotToken) == "" {
		return fmt.Errorf("telegram.bot_token 未配置")
	}
	if strings.TrimSpace(c.Telegram.ChatID) == "" {
		return fmt.Errorf("telegram.chat_id 未配置")
	}
	for i, u := range c.Users {
		if strings.TrimSpace(u.AK) == "" {
			return fmt.Errorf("users[%d] 缺少 ak", i)
		}
		if strings.TrimSpace(u.SK) == "" {
			return fmt.Errorf("users[%d] 缺少 sk", i)
		}
		if strings.TrimSpace(u.Region) == "" {
			return fmt.Errorf("users[%d] 缺少 region", i)
		}
		if strings.TrimSpace(u.InstanceID) == "" {
			return fmt.Errorf("users[%d] 缺少 instance_id", i)
		}
		if u.TrafficLimit <= 0 {
			return fmt.Errorf("users[%d] traffic_limit 必须大于 0", i)
		}
	}
	return nil
}

// SortUsers 按名称排序用户列表（输出稳定，便于比较）。
func SortUsers(users []UserConfig) {
	sort.SliceStable(users, func(i, j int) bool {
		return users[i].Name < users[j].Name
	})
}
