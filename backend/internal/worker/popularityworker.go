package worker

import (
	"context"
	"encoding/json"
	"errors"
	"feedsystem_video_go/internal/middleware/rabbitmq"
	rediscache "feedsystem_video_go/internal/middleware/redis"
	"log"
	"strconv"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

type PopularityWorker struct {
	ch         *amqp.Channel
	cache      *rediscache.Client
	queue      string
	quarantine func(context.Context, amqp.Delivery) error
}

func NewPopularityWorker(ch *amqp.Channel, cache *rediscache.Client, queue string) *PopularityWorker {
	return &PopularityWorker{ch: ch, cache: cache, queue: queue}
}

func (w *PopularityWorker) Run(ctx context.Context) error {
	if w == nil || w.ch == nil || w.cache == nil {
		return errors.New("popularity worker is not initialized")
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

func (w *PopularityWorker) handleDelivery(ctx context.Context, d amqp.Delivery) error {
	return handleDelivery(ctx, d, func(ctx context.Context) error { return w.processDelivery(ctx, d) }, w.quarantine)
}

func (w *PopularityWorker) process(ctx context.Context, body []byte) error {
	return w.processDelivery(ctx, amqp.Delivery{Body: body})
}

func (w *PopularityWorker) processDelivery(ctx context.Context, d amqp.Delivery) error {
	var evt rabbitmq.PopularityEvent
	if err := json.Unmarshal(d.Body, &evt); err != nil {
		return err
	}
	if evt.VideoID == 0 || evt.Change == 0 {
		return nil
	}
	at := evt.OccurredAt
	if at.IsZero() {
		at = d.Timestamp
	}
	if at.IsZero() {
		at = time.Now()
	}
	window := at.UTC().Truncate(time.Minute)
	eventID := deliveryEventID(d, evt.EventID)
	opCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	err := w.cache.ApplyPopularityEvent(opCtx, eventID,
		w.cache.Key("hot:video:1m:%s", window.Format("200601021504")), strconv.FormatUint(uint64(evt.VideoID), 10),
		w.cache.Key("video:detail:id=%d", evt.VideoID), evt.Change, window.Add(popularityWindowRetention))
	// A delivery older than the retention targets a minute bucket the feed no
	// longer reads, so there is nothing to rank. MySQL already holds the
	// authoritative counter: acknowledge the event instead of retrying it, but
	// leave a trace so a broker outage that grows the lag is observable.
	if errors.Is(err, rediscache.ErrPopularityWindowExpired) {
		log.Printf("popularity: skipped event %s for video %d: its %s hot bucket expired before delivery; the MySQL counter stays authoritative",
			eventID, evt.VideoID, window.Format("2006-01-02 15:04"))
		return nil
	}
	return err
}

// popularityWindowRetention is how long a one-minute hot bucket stays readable
// after the minute it describes. Feed queries merge the last 60 minutes, so
// anything older can only be a late replay.
const popularityWindowRetention = 2 * time.Hour
