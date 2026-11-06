package worker

import (
	"context"
	"log"
	"time"

	"feedsystem_video_go/internal/config"
	"feedsystem_video_go/internal/middleware/rabbitmq"
	rediscache "feedsystem_video_go/internal/middleware/redis"
	"feedsystem_video_go/internal/social"
	"feedsystem_video_go/internal/video"
	amqp "github.com/rabbitmq/amqp091-go"
	"gorm.io/gorm"
)

type consumer struct {
	queue string
	run   func(context.Context, *amqp.Channel) error
}

// A broker restart closes deliveries. Re-establish all consumers rather than
// leaving a live process with no consumers (or requiring a manual restart).
func supervise(ctx context.Context, cfg config.RabbitMQConfig, consumers []consumer) {
	for ctx.Err() == nil {
		err := consumeGroup(ctx, cfg, consumers)
		if ctx.Err() != nil {
			return
		}
		log.Printf("MQ consumers reconnecting: %v", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

func consumeGroup(ctx context.Context, cfg config.RabbitMQConfig, consumers []consumer) error {
	mq, err := rabbitmq.NewRabbitMQ(&cfg)
	if err != nil {
		return err
	}
	defer mq.Close()
	if err := rabbitmq.DeclareReliableTopology(mq.Ch); err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, len(consumers))
	for _, c := range consumers {
		ch, err := mq.NewChannel()
		if err != nil {
			return err
		}
		if err := ch.Qos(50, 0, false); err != nil {
			return err
		}
		go func(c consumer, ch *amqp.Channel) { done <- c.run(runCtx, ch) }(c, ch)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-done:
		return err
	}
}

func RunNotifications(ctx context.Context, cfg config.RabbitMQConfig, db *gorm.DB, hub NotificationHub) {
	var consumers []consumer
	for _, queue := range []string{"notification.like", "notification.comment", "notification.social"} {
		queue := queue
		consumers = append(consumers, consumer{queue: queue, run: func(ctx context.Context, ch *amqp.Channel) error {
			return NewNotificationWorker(ch, db, queue, hub).Run(ctx)
		}})
	}
	supervise(ctx, cfg, consumers)
}

func RunInteractionConsumers(ctx context.Context, cfg config.RabbitMQConfig, db *gorm.DB, cache *rediscache.Client) {
	supervise(ctx, cfg, []consumer{
		{queue: "like.events", run: func(ctx context.Context, ch *amqp.Channel) error {
			return NewLikeWorker(ch, video.NewLikeRepository(db), video.NewVideoRepository(db), "like.events").Run(ctx)
		}},
		{queue: "comment.events", run: func(ctx context.Context, ch *amqp.Channel) error {
			return NewCommentWorker(ch, video.NewCommentRepository(db), video.NewVideoRepository(db), "comment.events").Run(ctx)
		}},
		{queue: "social.events", run: func(ctx context.Context, ch *amqp.Channel) error {
			return NewSocialWorker(ch, social.NewSocialRepository(db), "social.events").Run(ctx)
		}},
		{queue: "video.popularity.events", run: func(ctx context.Context, ch *amqp.Channel) error {
			return NewPopularityWorker(ch, cache, "video.popularity.events").Run(ctx)
		}},
	})
}
