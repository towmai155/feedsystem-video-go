package worker

import (
	"context"
	"encoding/json"
	"errors"
	"feedsystem_video_go/internal/middleware/rabbitmq"
	"feedsystem_video_go/internal/video"
	"strings"

	amqp "github.com/rabbitmq/amqp091-go"
	"gorm.io/gorm"
)

type CommentWorker struct {
	ch         *amqp.Channel
	comments   *video.CommentRepository
	videos     *video.VideoRepository
	queue      string
	quarantine func(context.Context, amqp.Delivery) error
}

func NewCommentWorker(ch *amqp.Channel, comments *video.CommentRepository, videos *video.VideoRepository, queue string) *CommentWorker {
	return &CommentWorker{ch: ch, comments: comments, videos: videos, queue: queue}
}

func (w *CommentWorker) Run(ctx context.Context) error {
	if w == nil || w.ch == nil || w.comments == nil || w.videos == nil {
		return errors.New("comment worker is not initialized")
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

func (w *CommentWorker) handleDelivery(ctx context.Context, d amqp.Delivery) error {
	return handleDelivery(ctx, d, func(ctx context.Context) error { return w.process(ctx, d.Body) }, w.quarantine)
}

func (w *CommentWorker) process(ctx context.Context, body []byte) error {
	var evt rabbitmq.CommentEvent
	if err := json.Unmarshal(body, &evt); err != nil {
		return err
	}
	switch evt.Action {
	case "publish":
		return w.applyPublish(ctx, &evt)
	case "delete":
		return w.applyDelete(ctx, &evt)
	default:
		return nil
	}
}

func (w *CommentWorker) applyPublish(ctx context.Context, evt *rabbitmq.CommentEvent) error {
	if evt == nil || evt.VideoID == 0 || evt.AuthorID == 0 || strings.TrimSpace(evt.Content) == "" {
		return nil
	}
	// 新一代事件已经在 HTTP 事务里写过评论和热度，这里再写一次就会产生重复评论。
	if evt.Applied {
		return nil
	}

	c := &video.Comment{
		Username: strings.TrimSpace(evt.Username),
		VideoID:  evt.VideoID,
		AuthorID: evt.AuthorID,
		Content:  strings.TrimSpace(evt.Content),
	}
	if evt.EventID != "" {
		eventID := evt.EventID
		c.EventID = &eventID
	}
	return w.videos.Transaction(ctx, func(tx *gorm.DB) error {
		ok, err := w.videos.IsExistTx(tx, evt.VideoID)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		if err := w.comments.CreateCommentTx(tx, c); err != nil {
			// 同一事件在提交后、ACK 前被重投：评论和热度已经在第一次事务里写入。
			if isDuplicateKey(err) {
				return nil
			}
			return err
		}
		return w.videos.ChangePopularityTx(tx, evt.VideoID, 1)
	})
}

func (w *CommentWorker) applyDelete(ctx context.Context, evt *rabbitmq.CommentEvent) error {
	if evt == nil || evt.CommentID == 0 {
		return nil
	}
	c, err := w.comments.GetByID(ctx, evt.CommentID)
	if err != nil {
		return err
	}
	if c == nil {
		return nil
	}
	return w.comments.DeleteComment(ctx, c)
}
