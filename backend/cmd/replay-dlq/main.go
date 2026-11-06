// replay-dlq replays a bounded number of this application's quarantined events.
// The original is ACKed only after confirmed republication with the same ID.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"feedsystem_video_go/internal/config"
	"feedsystem_video_go/internal/middleware/rabbitmq"
	amqp "github.com/rabbitmq/amqp091-go"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	queue := flag.String("queue", "", "source queue, e.g. video.popularity.events")
	limit := flag.Int("limit", 10, "maximum messages to replay (1..100)")
	flag.Parse()
	allowed := map[string]struct{ exchange, prefix string }{
		"video.popularity.events": {"video.popularity.events", "video.popularity."},
		"like.events":             {"like.events", "like."}, "notification.like": {"like.events", "like."},
		"comment.events": {"comment.events", "comment."}, "notification.comment": {"comment.events", "comment."},
		"social.events": {"social.events", "social."}, "notification.social": {"social.events", "social."},
	}
	expected, ok := allowed[*queue]
	if !ok || *limit < 1 || *limit > 100 {
		return fmt.Errorf("supply a known -queue and -limit 1..100")
	}
	path := os.Getenv("CONFIG_PATH")
	if path == "" {
		path = "configs/config.yaml"
	}
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	mq, err := rabbitmq.NewRabbitMQ(&cfg.RabbitMQ)
	if err != nil {
		return err
	}
	defer mq.Close()
	publisher := rabbitmq.NewConfirmedPublisher(cfg.RabbitMQ)
	defer publisher.Close()
	for i := 0; i < *limit; i++ {
		d, ok, err := mq.Ch.Get(*queue+".dlx", false)
		if err != nil {
			return err
		}
		if !ok {
			fmt.Printf("replayed %d messages; DLQ is empty\n", i)
			return nil
		}
		exchange, _ := d.Headers["x-original-exchange"].(string)
		key, _ := d.Headers["x-original-routing-key"].(string)
		if d.Headers["x-original-queue"] != *queue || exchange != expected.exchange || !strings.HasPrefix(key, expected.prefix) {
			return fmt.Errorf("source metadata does not match this queue; original message left unacknowledged")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err = publisher.Publish(ctx, exchange, key, amqp.Publishing{MessageId: d.MessageId, ContentType: d.ContentType, DeliveryMode: amqp.Persistent, Timestamp: d.Timestamp, Headers: d.Headers, Body: d.Body})
		cancel()
		if err != nil {
			return fmt.Errorf("publish failed; original retained: %w", err)
		}
		if err := d.Ack(false); err != nil {
			return fmt.Errorf("ACK failed; a later replay may redeliver safely: %w", err)
		}
		fmt.Printf("replayed %s event=%s\n", *queue, d.MessageId)
	}
	return nil
}
