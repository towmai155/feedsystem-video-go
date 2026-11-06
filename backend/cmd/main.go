package main

import (
	"context"
	"feedsystem_video_go/internal/config"
	"feedsystem_video_go/internal/db"
	"feedsystem_video_go/internal/events"
	apphttp "feedsystem_video_go/internal/http"
	rabbitmq "feedsystem_video_go/internal/middleware/rabbitmq"
	rediscache "feedsystem_video_go/internal/middleware/redis"
	"feedsystem_video_go/internal/observability"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/joho/godotenv"
)

func main() {
	// 加载 .env（本地开发）
	if err := godotenv.Load(); err != nil {
		log.Println(".env not found; continuing")
	}

	// 加载配置
	configPath := os.Getenv("CONFIG_PATH")
	if configPath == "" {
		configPath = "configs/config.yaml"
	}
	log.Printf("Loading config from %s", configPath)
	cfg, usedDefault, err := config.LoadLocalDev(configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	if usedDefault {
		log.Printf("Config File %s not found, using default local config", configPath)
	} else {
		log.Printf("Config loaded from file: %s", configPath)
	}

	// 连接数据库
	//log.Printf("Database config: %v", cfg.Database)
	sqlDB, err := db.NewDB(cfg.Database)
	if err != nil {
		log.Fatalf("Failed to connect database: %v", err)
	}
	if err := db.AutoMigrate(sqlDB); err != nil {
		log.Fatalf("Failed to auto migrate database: %v", err)
	}
	defer db.CloseDB(sqlDB)

	// 会话校验专用小连接池：与主池隔离，写入突发时鉴权仍能拿到连接。
	sessionDB, err := db.NewSessionDB(cfg.Database)
	if err != nil {
		log.Fatalf("Failed to open session database pool: %v", err)
	}
	defer db.CloseDB(sessionDB)

	// 连接 Redis (可选，用于缓存)
	cache, err := rediscache.NewFromEnv(&cfg.Redis)
	if err != nil {
		log.Printf("Redis config error (cache disabled): %v", err)
		cache = nil
	} else {
		pingCtx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		if err := cache.Ping(pingCtx); err != nil {
			log.Printf("Redis not available (cache disabled): %v", err)
			_ = cache.Close()
			cache = nil
		} else {
			defer cache.Close()
			log.Printf("Redis connected (cache enabled)")
		}
	}

	// 连接 RabbitMQ (可选，用于消息队列)
	rmq, err := rabbitmq.NewRabbitMQ(&cfg.RabbitMQ)
	if err != nil {
		log.Printf("RabbitMQ config error (disabled): %v", err)
		rmq = nil
	} else {
		defer rmq.Close()
		log.Printf("RabbitMQ connected")
	}
	// Pprof
	connectionPool, err := sqlDB.DB()
	if err != nil {
		log.Fatalf("Failed to access database pool: %v", err)
	}
	pprofServer, err := observability.NewPprofServer(
		"API",
		cfg.ObservabilityConfig.Pprof.Enabled,
		cfg.ObservabilityConfig.Pprof.ApiAddr,
		connectionPool,
	)
	if err != nil {
		log.Printf("Failed to start API pprof server: %v", err)
	}
	if pprofServer != nil {
		defer pprofServer.Close()
	}

	// 设置路由
	r := apphttp.SetRouter(sqlDB, sessionDB, cache, rmq, &cfg.RabbitMQ)
	outboxCtx, stopOutbox := context.WithCancel(context.Background())
	publisher := rabbitmq.NewConfirmedPublisher(cfg.RabbitMQ)
	defer func() { stopOutbox(); publisher.Close() }()
	go (&events.Dispatcher{DB: sqlDB, Publisher: publisher}).Run(outboxCtx)

	// 显式构造 http.Server，补上 gin 默认引擎缺的头部/空闲超时：
	// 慢客户端或半死连接否则会一直占着 goroutine 和连接。
	// 注意不能设 WriteTimeout——/notification/stream 是长连接 SSE，
	// 写超时到点会直接掐断事件流。
	srv := &http.Server{
		Addr:              ":" + strconv.Itoa(cfg.Server.Port),
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	log.Printf("Server is running on port %d", cfg.Server.Port)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Failed to run server: %v", err)
	}
}
