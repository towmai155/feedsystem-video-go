package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Retry the held delivery, rather than immediately requeueing it. x-death only
// counts dead-lettering, not ordinary requeues. Exhaustion preserves the message
// in the configured DLX; cancellation leaves it available for a later consumer.
func retryDelivery(ctx context.Context, d amqp.Delivery, process func(context.Context) error, delays []time.Duration, quarantine ...func(context.Context, amqp.Delivery) error) error {
	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil {
			_ = d.Nack(false, true)
			return ctx.Err()
		}
		err := process(ctx)
		if err == nil {
			return d.Ack(false)
		}
		if ctx.Err() != nil {
			_ = d.Nack(false, true)
			return ctx.Err()
		}
		if attempt >= len(delays) {
			log.Printf("consumer failed after %d attempts; dead lettering: %v", attempt+1, err)
			if len(quarantine) > 0 && quarantine[0] != nil {
				if err := quarantine[0](ctx, d); err != nil {
					return err
				}
				return d.Ack(false)
			}
			return d.Nack(false, false)
		}
		log.Printf("consumer retry %d/%d: %v", attempt+1, len(delays), err)
		timer := time.NewTimer(delays[attempt])
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = d.Nack(false, true)
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func handleDelivery(ctx context.Context, d amqp.Delivery, process func(context.Context) error, quarantine ...func(context.Context, amqp.Delivery) error) error {
	return retryDelivery(ctx, d, process, []time.Duration{250 * time.Millisecond, time.Second, 3 * time.Second}, quarantine...)
}

func deliveryEventID(d amqp.Delivery, bodyID string) string {
	if bodyID != "" {
		return bodyID
	}
	if d.MessageId != "" {
		return d.MessageId
	}
	// Compatibility for historical events without IDs. Identical historical
	// payloads cannot be distinguished; all new producers emit an explicit ID.
	sum := sha256.Sum256(append([]byte(d.Exchange+"\x00"+d.RoutingKey+"\x00"), d.Body...))
	return "legacy-" + hex.EncodeToString(sum[:])
}
