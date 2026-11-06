package worker

import (
	"context"
	"encoding/json"
	"errors"
	"feedsystem_video_go/internal/middleware/rabbitmq"
	"feedsystem_video_go/internal/video"
	amqp "github.com/rabbitmq/amqp091-go"
	"gorm.io/gorm"
	"time"
)

type LikeWorker struct {
	ch         *amqp.Channel
	likes      *video.LikeRepository
	videos     *video.VideoRepository
	queue      string
	quarantine func(context.Context, amqp.Delivery) error
}

func NewLikeWorker(ch *amqp.Channel, likes *video.LikeRepository, videos *video.VideoRepository, queue string) *LikeWorker {
	return &LikeWorker{ch: ch, likes: likes, videos: videos, queue: queue}
}

func (w *LikeWorker) Run(ctx context.Context) error {
	if w == nil || w.ch == nil || w.likes == nil || w.videos == nil {
		return errors.New("like worker is not initialized")
	}
	if w.queue == "" {
		return errors.New("queue is required")
	}

	var err error
	w.quarantine, err = rabbitmq.NewQuarantine(w.ch, w.queue)
	if err != nil {
		return err
	}
	deliveries, err := w.ch.Consume(
		w.queue,
		"",
		false,
		false,
		false,
		false,
		nil,
	)
	if err != nil {
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case d, ok := <-deliveries:
			if !ok {
				return errors.New("deliveries channel closed")
			}
			if err := w.handleDelivery(ctx, d); err != nil {
				return err
			}
		}
	}
}

func (w *LikeWorker) handleDelivery(ctx context.Context, d amqp.Delivery) error {
	return handleDelivery(ctx, d, func(ctx context.Context) error { return w.process(ctx, d.Body) }, w.quarantine)
}

func (w *LikeWorker) process(ctx context.Context, body []byte) error {
	var evt rabbitmq.LikeEvent
	if err := json.Unmarshal(body, &evt); err != nil {
		return err
	}
	if evt.UserID == 0 || evt.VideoID == 0 {
		return nil
	}
	// 新一代事件在 HTTP 事务里已经落库，这里只需确认并跳过，避免重复计数。
	// 旧消息没有该标记，仍按原来的异步落库处理。
	if evt.Applied {
		return nil
	}

	switch evt.Action {
	case "like":
		return w.applyLike(ctx, evt.UserID, evt.VideoID)
	case "unlike":
		return w.applyUnlike(ctx, evt.UserID, evt.VideoID)
	default:
		return nil
	}
}

func (w *LikeWorker) applyLike(ctx context.Context, userID, videoID uint) error {
	return w.videos.Transaction(ctx, func(tx *gorm.DB) error {
		ok, err := w.videos.IsExistTx(tx, videoID)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}

		created, err := w.likes.LikeIgnoreDuplicateTx(tx, &video.Like{
			VideoID:   videoID,
			AccountID: userID,
			CreatedAt: time.Now(),
		})
		if err != nil {
			return err
		}
		// The row already exists, so its counters were committed with it.
		if !created {
			return nil
		}

		if err := w.videos.ChangeLikesCountTx(tx, videoID, 1); err != nil {
			return err
		}
		return w.videos.ChangePopularityTx(tx, videoID, 1)
	})
}

func (w *LikeWorker) applyUnlike(ctx context.Context, userID, videoID uint) error {
	return w.videos.Transaction(ctx, func(tx *gorm.DB) error {
		ok, err := w.videos.IsExistTx(tx, videoID)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}

		deleted, err := w.likes.DeleteByVideoAndAccountTx(tx, videoID, userID)
		if err != nil {
			return err
		}
		if !deleted {
			return nil
		}

		if err := w.videos.ChangeLikesCountTx(tx, videoID, -1); err != nil {
			return err
		}
		return w.videos.ChangePopularityTx(tx, videoID, -1)
	})
}
