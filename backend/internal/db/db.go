package db

import (
	"feedsystem_video_go/internal/account"
	"feedsystem_video_go/internal/config"
	"feedsystem_video_go/internal/events"
	"feedsystem_video_go/internal/message"
	"feedsystem_video_go/internal/social"
	"feedsystem_video_go/internal/video"
	"feedsystem_video_go/internal/worker"
	"fmt"
	"os"
	"strconv"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// API 与 Worker 各有独立连接池；部署时需按所有进程的总连接数留出数据库预算。
// 显式上限让高并发请求等待复用连接，避免无界建连；保留空闲连接减少重连。
// 连接拒绝的具体原因仍须结合运行指标验证，不能仅凭错误信息认定端口耗尽。
const (
	defaultMaxOpenConns        = 64
	defaultSessionMaxOpenConns = 16
	defaultConnMaxLifetime     = 30 * time.Minute
	defaultConnMaxIdleTime     = 5 * time.Minute
)

func envInt(name string, fallback int) int {
	if raw := os.Getenv(name); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			return v
		}
	}
	return fallback
}

func NewDB(dbcfg config.DatabaseConfig) (*gorm.DB, error) {
	return openDB(dbcfg, envInt("MYSQL_MAX_OPEN_CONNS", defaultMaxOpenConns))
}

// NewSessionDB 开一个独立的小连接池，只服务"每请求一次"的会话校验查询。
// 写入与 feed 读再多，也不会占满这个池，鉴权因此不会被饿到 2s 超时
// （压测里表现为 503 authentication temporarily unavailable）。
// 与主池相比它只是连接配额不同，走的还是同一个库、同一条 DSN。
func NewSessionDB(dbcfg config.DatabaseConfig) (*gorm.DB, error) {
	return openDB(dbcfg, envInt("MYSQL_SESSION_MAX_OPEN_CONNS", defaultSessionMaxOpenConns))
}

func openDB(dbcfg config.DatabaseConfig, maxOpen int) (*gorm.DB, error) {
	// interpolateParams=true：普通查询在客户端完成参数替换，省掉每条语句
	// PREPARE/EXECUTE/CLOSE 的额外往返（鉴权、点赞这类简单查询收益最大）。
	// timeout/readTimeout/writeTimeout：网络或服务端异常时让语句有界失败，
	// 而不是把池里的连接一直挂住。
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?charset=utf8mb4&parseTime=True&loc=Local&interpolateParams=true&timeout=5s&readTimeout=10s&writeTimeout=10s",
		dbcfg.User, dbcfg.Password, dbcfg.Host, dbcfg.Port, dbcfg.DBName)

	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		return nil, err
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	maxIdle := envInt("MYSQL_MAX_IDLE_CONNS", maxOpen)
	if maxIdle > maxOpen {
		maxIdle = maxOpen
	}
	sqlDB.SetMaxOpenConns(maxOpen)
	sqlDB.SetMaxIdleConns(maxIdle)
	sqlDB.SetConnMaxLifetime(defaultConnMaxLifetime)
	sqlDB.SetConnMaxIdleTime(defaultConnMaxIdleTime)

	return db, nil
}

func AutoMigrate(db *gorm.DB) error {
	return db.AutoMigrate(
		&account.Account{}, &video.Video{}, &video.Like{}, &video.Comment{},
		&social.Social{}, &video.OutboxMsg{}, &video.Tag{}, &video.VideoTag{},
		&message.Message{}, &worker.Notification{}, &events.OutboxEvent{},
	)
}

func CloseDB(db *gorm.DB) error {
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}
