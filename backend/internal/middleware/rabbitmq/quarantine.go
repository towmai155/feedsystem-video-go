package rabbitmq

import (
	"context"
	"errors"
	"fmt"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Quarantine publishes to this consumer's DLQ through the default direct
// exchange. It avoids the historical shared DLX's '#' fanout to unrelated DLQs.
// The caller ACKs the original only after this publication is confirmed.
func NewQuarantine(ch *amqp.Channel, queue string) (func(context.Context, amqp.Delivery) error, error) {
	if err := ch.Confirm(false); err != nil {
		return nil, err
	}
	returns := ch.NotifyReturn(make(chan amqp.Return, 1))
	return func(ctx context.Context, d amqp.Delivery) error {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		headers := amqp.Table{}
		for k, v := range d.Headers {
			headers[k] = v
		}
		headers["x-original-queue"] = queue
		headers["x-original-exchange"] = d.Exchange
		headers["x-original-routing-key"] = d.RoutingKey
		headers["x-retry-count"] = int32(MaxRetryCount)
		confirmation, err := ch.PublishWithDeferredConfirm("", queue+".dlx", true, false, amqp.Publishing{
			Headers: headers, MessageId: d.MessageId, ContentType: d.ContentType, DeliveryMode: amqp.Persistent, Timestamp: d.Timestamp, Body: d.Body,
		})
		if err != nil {
			return err
		}
		if confirmation == nil {
			return errors.New("DLQ publisher confirms unavailable")
		}
		ack, err := confirmation.WaitContext(ctx)
		if err != nil {
			return err
		}
		if !ack {
			return errors.New("DLQ publish rejected")
		}
		select {
		case returned, ok := <-returns:
			if !ok {
				return errors.New("DLQ return listener closed")
			}
			return fmt.Errorf("DLQ unroutable: %s", returned.ReplyText)
		default:
			return nil
		}
	}, nil
}
