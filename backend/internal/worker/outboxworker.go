package worker

import (
	"context"
	"encoding/json"
	"feedsystem_video_go/internal/middleware/rabbitmq"
	"feedsystem_video_go/internal/middleware/redis"
	"feedsystem_video_go/internal/video"
	"fmt"
	"log"
	"time"

	oredis "github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

const (
	timelineOutboxBatch        = 100
	timelineOutboxInterval     = time.Second
	timelineOutboxClaimTTL     = 5 * time.Minute
	timelineOutboxSweepEvery   = time.Minute
	timelineOutboxPublishLimit = 5 * time.Second
)

// StartOutboxPoller 每秒把待投递的 timeline 事件交给 MQ。
// 多实例部署时先用条件更新抢占（pending → sending），抢不到就跳过，避免同一行
// 被重复发布；进程崩溃留下的 sending 行在 timelineOutboxClaimTTL 之后回到 pending。
func StartOutboxPoller(db *gorm.DB, tmq *rabbitmq.TimelineMQ) {
	if db == nil || tmq == nil || tmq.RabbitMQ == nil || tmq.Ch == nil {
		log.Printf("Outbox poller disabled: timeline mq is not initialized")
		return
	}

	go func() {
		ticker := time.NewTicker(timelineOutboxInterval)
		defer ticker.Stop()
		lastSweep := time.Time{}
		for range ticker.C {
			if time.Since(lastSweep) >= timelineOutboxSweepEvery {
				lastSweep = time.Now()
				if err := releaseStaleTimelineOutbox(db); err != nil {
					log.Printf("timeline outbox: release stale claims: %v", err)
				}
			}
			dispatchTimelineOutbox(db, tmq)
		}
	}()
}

func releaseStaleTimelineOutbox(db *gorm.DB) error {
	return db.Model(&video.OutboxMsg{}).
		Where("status = ? AND (claimed_at IS NULL OR claimed_at < ?)", "sending", time.Now().Add(-timelineOutboxClaimTTL)).
		Updates(map[string]any{"status": "pending", "claimed_at": nil}).Error
}

// claimTimelineOutbox flips one pending row to sending. The conditional update is
// the only writer that can observe RowsAffected == 1, so two pollers never
// publish the same timeline event.
func claimTimelineOutbox(db *gorm.DB, id uint) (bool, error) {
	res := db.Model(&video.OutboxMsg{}).
		Where("id = ? AND status = ?", id, "pending").
		Updates(map[string]any{"status": "sending", "claimed_at": time.Now()})
	return res.RowsAffected == 1, res.Error
}

func dispatchTimelineOutbox(db *gorm.DB, tmq *rabbitmq.TimelineMQ) {
	var messages []video.OutboxMsg
	if err := db.Where("status = ?", "pending").Order("create_time ASC").
		Limit(timelineOutboxBatch).Find(&messages).Error; err != nil {
		log.Printf("timeline outbox: load pending rows: %v", err)
		return
	}
	for _, msg := range messages {
		claimed, err := claimTimelineOutbox(db, msg.ID)
		if err != nil {
			log.Printf("timeline outbox: claim row %d: %v", msg.ID, err)
			continue
		}
		if !claimed {
			continue // 另一个实例已经抢到这一行
		}

		ctx, cancel := context.WithTimeout(context.Background(), timelineOutboxPublishLimit)
		err = tmq.PublishVideo(ctx, msg.VideoID, msg.CreateTime)
		cancel()
		if err != nil {
			if rbErr := db.Model(&video.OutboxMsg{}).Where("id = ?", msg.ID).
				Updates(map[string]any{"status": "pending", "claimed_at": nil}).Error; rbErr != nil {
				log.Printf("timeline outbox: release row %d: %v", msg.ID, rbErr)
			}
			log.Printf("投递MQ失败: VideoID: %d, err: %v", msg.VideoID, err)
			continue
		}
		if err := db.Delete(&msg).Error; err != nil {
			log.Printf("timeline outbox: delete published row %d: %v", msg.ID, err)
		}
	}
}

func StartConsumer(tmq *rabbitmq.TimelineMQ, queueName string, redisClient *redis.Client) {
	if tmq == nil || tmq.RabbitMQ == nil || tmq.Ch == nil {
		log.Printf("Timeline consumer disabled: timeline mq is not initialized")
		return
	}
	if redisClient == nil {
		log.Printf("Timeline consumer disabled: redis is not initialized")
		return
	}

	// 专用 channel：共享 channel 上同时 Consume 与 Publish 会让帧交错，而且
	// Qos 是按 channel 生效的。
	ch, err := tmq.RabbitMQ.NewChannel()
	if err != nil {
		log.Printf("Timeline consumer disabled: open channel: %v", err)
		return
	}
	if err := ch.Qos(50, 0, false); err != nil {
		log.Printf("Timeline consumer disabled: set qos: %v", err)
		return
	}

	msgs, err := ch.Consume(
		queueName,
		"",
		false,
		false,
		false,
		false,
		nil,
	)
	if err != nil {
		log.Printf("注册消费失败: %v", err)
		return
	}

	go func() {
		for msg := range msgs {
			var event rabbitmq.TimelineEvent
			if err := json.Unmarshal(msg.Body, &event); err != nil {
				log.Printf("反序列化失败: %v", err)
				_ = msg.Ack(false)
				continue
			}

			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			timelineKey := redisClient.Key("feed:global_timeline")
			err := redisClient.ZAdd(ctx, timelineKey, oredis.Z{
				Score:  float64(event.CreateTime),
				Member: fmt.Sprintf("%d", event.VideoID),
			})
			if err == nil {
				// 修剪是尽力而为：失败不能让已经写入的成员反复重投。
				if trimErr := redisClient.ZRemRangeByRank(ctx, timelineKey, 0, -1001); trimErr != nil {
					log.Printf("ZRem失败: %v", trimErr)
				}
				cancel()
				_ = msg.Ack(false)
				continue
			}
			cancel()

			// Redis 故障时立刻 requeue 会变成热循环，先退避再重投。
			log.Printf("写入Zset失败: %v", err)
			time.Sleep(200 * time.Millisecond)
			_ = msg.Nack(false, true)
		}
	}()
}
