package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

// DefaultDataDir 返回数据目录（环境变量 ALIYUN_MONITOR_DATA，默认 /data）。
func DefaultDataDir() string {
	if d := os.Getenv("ALIYUN_MONITOR_DATA"); d != "" {
		return d
	}
	return "/data"
}

// ConfigPath 返回 config.json 的路径。设置了 --config 时优先使用该路径。
func ConfigPath() string {
	if ConfigOverride != "" {
		return ConfigOverride
	}
	return filepath.Join(DefaultDataDir(), "config.json")
}

// Load 从磁盘加载配置。config.json 不存在或格式非法时返回错误。
func Load() (*Config, error) {
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
