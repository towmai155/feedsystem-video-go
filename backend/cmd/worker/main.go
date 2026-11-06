package main

import (
	"context"
	"feedsystem_video_go/internal/config"
	"feedsystem_video_go/internal/db"
	"feedsystem_video_go/internal/events"
	"feedsystem_video_go/internal/middleware/rabbitmq"
	rediscache "feedsystem_video_go/internal/middleware/redis"
	"feedsystem_video_go/internal/observability"
	"feedsystem_video_go/internal/worker"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gorm.io/gorm"
)

func connectWithRetry(name string, maxRetries int, fn func() error) {
	for i := 0; i < maxRetries; i++ {
		if err := fn(); err == nil {
			return
		}
		wait := min(time.Duration(1<<i)*time.Second, 30*time.Second)
		log.Printf("%s unavailable; retrying in %v (%d/%d)", name, wait, i+1, maxRetries)
		time.Sleep(wait)
	}
	log.Fatalf("%s: maximum startup retries exceeded", name)
}

func main() {
	configPath := os.Getenv("CONFIG_PATH")
	if configPath == "" {
		configPath = "configs/config.yaml"
	}
	cfg, _, err := config.LoadLocalDev(configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	var sqlDB *gorm.DB
	connectWithRetry("MySQL", 10, func() error {
		var err error
		sqlDB, err = db.NewDB(cfg.Database)
		return err
	})
	defer db.CloseDB(sqlDB)
	// Both entry points initialize new reliability tables, so startup order is
	// irrelevant and a standalone worker can recover after an API outage.
	if err := db.AutoMigrate(sqlDB); err != nil {
		log.Fatalf("migrate worker schema: %v", err)
	}
	cache, err := rediscache.NewFromEnv(&cfg.Redis)
	if err != nil {
		log.Fatalf("Redis config: %v", err)
	}
	defer cache.Close()
	// Keep the client even if Redis is temporarily unavailable at startup.
	// Consumers retry failures and preserve exhausted deliveries in the DLX.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pool, err := sqlDB.DB()
	if err != nil {
		log.Fatalf("access SQL pool: %v", err)
	}
	profile, err := observability.NewPprofServer("Worker", cfg.ObservabilityConfig.Pprof.Enabled, cfg.ObservabilityConfig.Pprof.WorkerAddr, pool)
	if err != nil {
		log.Printf("Worker pprof: %v", err)
	}
	if profile != nil {
		defer profile.Close()
	}
	// The outbox dispatcher also runs here: when every API instance is down the
	// worker must still publish committed interaction events instead of leaving
	// them stranded in outbox_events. Leases keep the two processes from colliding.
	publisher := rabbitmq.NewConfirmedPublisher(cfg.RabbitMQ)
	defer publisher.Close()
	go (&events.Dispatcher{DB: sqlDB, Publisher: publisher}).Run(ctx)
	log.Printf("Worker started; consumers reconnect automatically")
	worker.RunInteractionConsumers(ctx, cfg.RabbitMQ, sqlDB, cache)
}
