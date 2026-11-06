package worker

import (
	"context"
	"encoding/json"
	"errors"
	"feedsystem_video_go/internal/middleware/rabbitmq"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Notification struct {
	ID          uint      `gorm:"primaryKey" json:"id"`
	RecipientID uint      `gorm:"index;not null" json:"recipient_id"`
	SenderID    uint      `gorm:"not null" json:"sender_id"`
	Type        string    `gorm:"type:varchar(50);not null" json:"type"`
	TargetID    uint      `json:"target_id"`
	Content     string    `gorm:"type:varchar(255)" json:"content"`
	IsRead      bool      `gorm:"default:false" json:"is_read"`
	CreatedAt   time.Time `gorm:"autoCreateTime" json:"created_at"`
	// NULL preserves historical rows while new events receive a unique ID.
	EventID *string `gorm:"type:varchar(96);uniqueIndex:idx_notification_event" json:"-"`
}

type NotificationWorker struct {
	ch         *amqp.Channel
	db         *gorm.DB
	queue      string
	hub        NotificationHub
	quarantine func(context.Context, amqp.Delivery) error
}

type NotificationHub interface {
	Push(userID uint, n *Notification)
}

func NewNotificationWorker(ch *amqp.Channel, db *gorm.DB, queue string, hub NotificationHub) *NotificationWorker {
	return &NotificationWorker{ch: ch, db: db, queue: queue, hub: hub}
}

func (w *NotificationWorker) Run(ctx context.Context) error {
	if w == nil || w.ch == nil || w.db == nil {
		return errors.New("notification worker is not initialized")
	}
	// The table is created once by db.AutoMigrate at process start. Migrating it
	// here ran concurrent CREATE TABLE from every consumer goroutine, so a DDL
	// clash killed all of them and the supervisor reconnected once per second.
	var err error
	w.quarantine, err = rabbitmq.NewQuarantine(w.ch, w.queue)
	if err != nil {
		return err
	}
	deliveries, err := w.ch.Consume(w.queue, "", false, false, false, false, nil)
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

func (w *NotificationWorker) handleDelivery(ctx context.Context, d amqp.Delivery) error {
	return handleDelivery(ctx, d, func(ctx context.Context) error { return w.process(ctx, d) }, w.quarantine)
}

// videoAuthorID 查询视频作者 id：查询失败返回 error 让消息重试，视频不存在返回 (0, nil)。
// 之前这里丢弃了 Scan 错误，连接抖动时 authorID=0 被当成"无收件人"，消息被 ACK 后永久丢失。
func (w *NotificationWorker) videoAuthorID(ctx context.Context, videoID uint) (uint, error) {
	var row struct{ AuthorID uint }
	if err := w.db.WithContext(ctx).Table("videos").
		Select("author_id").Where("id = ?", videoID).Scan(&row).Error; err != nil {
		return 0, err
	}
	return row.AuthorID, nil
}

func (w *NotificationWorker) process(ctx context.Context, d amqp.Delivery) error {
	body := d.Body
	if len(body) == 0 {
		return nil
	}
	routingKey := d.RoutingKey

	var notif *Notification
	var eventID string

	switch {
	case routingKey == "like.like":
		var evt rabbitmq.LikeEvent
		if err := json.Unmarshal(body, &evt); err != nil {
			return err
		}
		eventID = deliveryEventID(d, evt.EventID)
		if evt.UserID == 0 || evt.VideoID == 0 {
			return nil
		}
		var authorID uint
		authorID, err := w.videoAuthorID(ctx, evt.VideoID)
		if err != nil {
			// 查询失败必须重试：以前这里吞掉错误、带着 authorID=0 直接 ACK，通知就永久丢了。
			return err
		}
		if authorID == 0 || authorID == evt.UserID {
			return nil
		}
		notif = &Notification{RecipientID: authorID, SenderID: evt.UserID, Type: "like", TargetID: evt.VideoID, Content: "点赞了你的视频"}

	case routingKey == "comment.publish":
		var evt rabbitmq.CommentEvent
		if err := json.Unmarshal(body, &evt); err != nil {
			return err
		}
		eventID = deliveryEventID(d, evt.EventID)
		if evt.AuthorID == 0 || evt.VideoID == 0 {
			return nil
		}
		var authorID uint
		authorID, err := w.videoAuthorID(ctx, evt.VideoID)
		if err != nil {
			return err
		}
		if authorID == 0 || authorID == evt.AuthorID {
			return nil
		}
		notif = &Notification{RecipientID: authorID, SenderID: evt.AuthorID, Type: "comment", TargetID: evt.VideoID, Content: "评论了你的视频"}

	case routingKey == "social.follow":
		var evt rabbitmq.SocialEvent
		if err := json.Unmarshal(body, &evt); err != nil {
			return err
		}
		eventID = deliveryEventID(d, evt.EventID)
		if evt.FollowerID == 0 || evt.VloggerID == 0 {
			return nil
		}
		notif = &Notification{RecipientID: evt.VloggerID, SenderID: evt.FollowerID, Type: "follow", TargetID: evt.FollowerID, Content: "关注了你"}
	}

	if notif == nil {
		return nil
	}
	notif.EventID = &eventID
	created := w.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(notif)
	if created.Error != nil {
		return created.Error
	}
	if created.RowsAffected > 0 && w.hub != nil {
		w.hub.Push(notif.RecipientID, notif)
	}
	return nil
}
