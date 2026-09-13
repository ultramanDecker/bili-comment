// Package config 负责配置目录与登录态的持久化。
//
// 位置遵循各平台惯例：
//
//	Linux   ~/.config/bili-comment/
//	macOS   ~/Library/Application Support/bili-comment/
//	Windows %AppData%\bili-comment\
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const appDir = "bili-comment"

// Config 是持久化的配置。目前只存登录态。
type Config struct {
	Cookies map[string]string `json:"cookies,omitempty"`
	SavedAt string            `json:"saved_at,omitempty"`
}

// Dir 返回配置目录（不保证已存在）。
func Dir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("无法定位配置目录：%w", err)
	}
	return filepath.Join(base, appDir), nil
}

// Path 返回配置文件路径。
func Path() (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "config.json"), nil
}

// Load 读取配置。文件不存在时返回空配置而非错误——首次运行是正常情况。
func Load() (*Config, error) {
	p, err := Path()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取配置失败：%w", err)
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("配置文件格式损坏（%s），可删除后重新登录：%w", p, err)
	}
	return &c, nil
}

// Save 写入配置。权限设为 0600；Windows 上 POSIX 权限位不生效，
// 但不影响其他平台的安全语义。
func Save(c *Config) error {
	d, err := Dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		return fmt.Errorf("创建配置目录失败：%w", err)
	}
	c.SavedAt = time.Now().Format(time.RFC3339)
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	p := filepath.Join(d, "config.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		return fmt.Errorf("写入配置失败：%w", err)
	}
	return nil
}

// Clear 删除本地登录态。
func Clear() error {
	p, err := Path()
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
