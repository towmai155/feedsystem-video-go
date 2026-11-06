package worker

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"feedsystem_video_go/internal/testutil"
	amqp "github.com/rabbitmq/amqp091-go"
)

type notificationAcknowledgement struct {
	acks, nacks int
	requeue     bool
}

type countingNotificationHub struct{ pushes atomic.Int32 }

func (h *countingNotificationHub) Push(uint, *Notification) { h.pushes.Add(1) }

func TestNotificationRedeliveryDoesNotDuplicateRowOrPush(t *testing.T) {
	db := testutil.NewMySQL(t)
	if err := db.Exec("CREATE TABLE videos (id BIGINT UNSIGNED PRIMARY KEY, author_id BIGINT UNSIGNED NOT NULL)").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO videos (id,author_id) VALUES (7,9)").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&Notification{}); err != nil {
		t.Fatal(err)
	}
	hub := &countingNotificationHub{}
	w := NewNotificationWorker(nil, db, "notification.like", hub)
	d := amqp.Delivery{RoutingKey: "like.like", Body: []byte(`{"event_id":"stable-like-event","video_id":7,"user_id":3}`)}
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := w.process(context.Background(), d); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	var count int64
	if err := db.Model(&Notification{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 || hub.pushes.Load() != 1 {
		t.Fatalf("duplicate notification: rows=%d pushes=%d", count, hub.pushes.Load())
	}
}

func (a *notificationAcknowledgement) Ack(uint64, bool) error { a.acks++; return nil }
func (a *notificationAcknowledgement) Nack(_ uint64, _ bool, requeue bool) error {
	a.nacks++
	a.requeue = requeue
	return nil
}
func (a *notificationAcknowledgement) Reject(_ uint64, requeue bool) error {
	a.nacks++
	a.requeue = requeue
	return nil
}

// A failed author lookup must leave the message available for retry. Once the
// database recovers, that delivery must create a notification before being ACKed.
func TestNotificationLookupFailureRequeuesThenRecovers(t *testing.T) {
	for _, routingKey := range []string{"like.like", "comment.publish"} {
		t.Run(routingKey, func(t *testing.T) {
			db := testutil.NewMySQL(t)
			if err := db.Exec("CREATE TABLE videos (id BIGINT UNSIGNED PRIMARY KEY, author_id BIGINT UNSIGNED NOT NULL)").Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Exec("INSERT INTO videos (id, author_id) VALUES (7, 9)").Error; err != nil {
				t.Fatal(err)
			}
			if err := db.AutoMigrate(&Notification{}); err != nil {
				t.Fatal(err)
			}
			ack := &notificationAcknowledgement{}
			delivery := amqp.Delivery{Acknowledger: ack, DeliveryTag: 1, RoutingKey: routingKey,
				Body: []byte(`{"video_id":7,"user_id":3,"author_id":3}`)}
			worker := NewNotificationWorker(nil, db, "test", nil)
			failedContext, cancel := context.WithCancel(context.Background())
			cancel()
			worker.handleDelivery(failedContext, delivery)
			if ack.acks != 0 || ack.nacks != 1 || !ack.requeue {
				t.Fatalf("failed query must requeue without ACK: %+v", ack)
			}
			worker.handleDelivery(context.Background(), delivery)
			if ack.acks != 1 || ack.nacks != 1 {
				t.Fatalf("recovered delivery must ACK: %+v", ack)
			}
			var notifications []Notification
			if err := db.Find(&notifications).Error; err != nil {
				t.Fatal(err)
			}
			if len(notifications) != 1 || notifications[0].RecipientID != 9 || notifications[0].SenderID != 3 || notifications[0].TargetID != 7 {
				t.Fatalf("recovered delivery must produce exactly one correct notification: %+v", notifications)
			}
		})
	}
}
