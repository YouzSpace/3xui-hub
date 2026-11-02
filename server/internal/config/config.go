// Package config 加载 .env 配置，与 Laravel 的 env() 命名保持一致。
//
// 默认读取仓库根 .env（install.sh 写入 APP_KEY / DB_*，与 PHP 后端历史配置同源），进程环境变量优先于文件值，
// 与 Laravel 的 env() 语义一致（已存在的环境变量不会被 .env 覆盖）。
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"controlhub-server/internal/crypto"

	"github.com/joho/godotenv"
)

// Config 面板运行所需的全部配置。
type Config struct {
	AppKey   []byte // AES-256-CBC 32 字节密钥（已从 base64: 解码）
	AppEnv   string
	AppDebug bool

	DB DBConfig

	Session SessionConfig

	BcryptRounds int
}

// DBConfig 数据库连接参数。全部来自 DB_* 环境变量。
type DBConfig struct {
	Host     string
	Port     string
	Database string
	Username string
	Password string
	// MaxOpenConns 连接池上限（DB_MAX_OPEN_CONNS，默认 50）。
	// 2C2G 小机上默认 20 在并发后台请求时会排队；调大并放开给部署侧按机器配置。
	MaxOpenConns int
	// MaxIdleConns 空闲连接上限（DB_MAX_IDLE_CONNS，默认 20）。
	MaxIdleConns int
}

// DSN 返回 go-sql-driver/mysql 连接串。
//
// 契约（contracts/data-contract.md §4）：Laravel timezone = UTC，所有 datetime 按 UTC 存；
// 因此必须 parseTime=true&loc=UTC，禁止混用本地时区。
func (d DBConfig) DSN() string {
	return fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?charset=utf8mb4&collation=utf8mb4_unicode_ci&parseTime=true&loc=UTC&clientFoundRows=true",
		d.Username, d.Password, d.Host, d.Port, d.Database)
}

// SessionConfig 管理端 session 配置。
type SessionConfig struct {
	Driver   string
	Lifetime time.Duration
}

// Load 从指定 .env 路径加载配置。
//
// path 为空时默认 ../.env（相对 server/ 工作目录，指向仓库根 .env）；文件不存在不是致命错误——
// 生产环境可能只注入环境变量，此时与 Laravel 一样以环境变量为准。
func Load(envPath string) (*Config, error) {
	if envPath == "" {
		envPath = DefaultEnvPath
	}
	// godotenv.Load 不覆盖已存在的环境变量，与 Laravel env() 一致。
	// 文件缺失时跳过（不静默忽略其他错误：格式错误仍然报错）。
	if err := godotenv.Load(envPath); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("加载 %s 失败: %w", envPath, err)
	}

	appKey, err := crypto.ParseAppKey(os.Getenv("APP_KEY"))
	if err != nil {
		// 最常见的原因是工作目录不对导致 .env 没加载到，报错里带上路径省去排查。
		return nil, fmt.Errorf("APP_KEY 无效（已尝试加载 %s，可用 CONFIG_ENV 指定路径）: %w",
			absOrSelf(envPath), err)
	}

	cfg := &Config{
		AppKey:   appKey,
		AppEnv:   envOr("APP_ENV", "production"),
		AppDebug: envBool("APP_DEBUG", false),
		DB: DBConfig{
			Host:     envOr("DB_HOST", "127.0.0.1"),
			Port:     envOr("DB_PORT", "3306"),
			Database: envOr("DB_DATABASE", "controlhub"),
			Username: envOr("DB_USERNAME", "root"),
			Password: os.Getenv("DB_PASSWORD"),

			MaxOpenConns: envInt("DB_MAX_OPEN_CONNS", 50),
			MaxIdleConns: envInt("DB_MAX_IDLE_CONNS", 20),
		},
		Session: SessionConfig{
			Driver: envOr("SESSION_DRIVER", "database"),
			// Laravel SESSION_LIFETIME 单位是分钟。
			Lifetime: time.Duration(envInt("SESSION_LIFETIME", 120)) * time.Minute,
		},
		BcryptRounds: envInt("BCRYPT_ROUNDS", 12),
	}

	// Laravel 的 Hash::make 默认 cost 12；bcrypt 只接受 4..31，越界会让登录校验静默失败。
	if cfg.BcryptRounds < 4 || cfg.BcryptRounds > 31 {
		return nil, fmt.Errorf("BCRYPT_ROUNDS=%d 非法（允许 4..31）", cfg.BcryptRounds)
	}

	return cfg, nil
}

// DefaultEnvPath 是 server/ 目录启动时使用的默认 .env 路径（仓库根 .env，install.sh 写入）。
const DefaultEnvPath = "../.env"

// absOrSelf 仅用于错误文案：拿到绝对路径便于定位，失败时退回原路径。
func absOrSelf(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}
