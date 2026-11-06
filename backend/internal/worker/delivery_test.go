package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

func TestDeliveryRetriesThenAcknowledgesRecovery(t *testing.T) {
	ack := &notificationAcknowledgement{}
	attempts := 0
	err := retryDelivery(context.Background(), amqp.Delivery{Acknowledger: ack}, func(context.Context) error {
		attempts++
		if attempts < 3 {
			return errors.New("temporary dependency failure")
		}
		return nil
	}, []time.Duration{0, 0, 0})
	if err != nil || attempts != 3 || ack.acks != 1 || ack.nacks != 0 {
		t.Fatalf("recovery: attempts=%d ack=%+v err=%v", attempts, ack, err)
	}
}

func TestExhaustedDeliveryRequiresConfirmedQuarantine(t *testing.T) {
	for _, quarantineFails := range []bool{true, false} {
		t.Run(map[bool]string{true: "failed-quarantine", false: "confirmed-quarantine"}[quarantineFails], func(t *testing.T) {
			ack := &notificationAcknowledgement{}
			attempts, quarantines := 0, 0
			err := retryDelivery(context.Background(), amqp.Delivery{Acknowledger: ack}, func(context.Context) error { attempts++; return errors.New("permanent dependency failure") }, []time.Duration{0, 0, 0}, func(context.Context, amqp.Delivery) error {
				quarantines++
				if quarantineFails {
					return errors.New("DLQ unavailable")
				}
				return nil
			})
			if attempts != 4 || quarantines != 1 || ack.nacks != 0 {
				t.Fatalf("unbounded retries or premature NACK: %d %d %+v", attempts, quarantines, ack)
			}
			if quarantineFails {
				if err == nil || ack.acks != 0 {
					t.Fatal("unconfirmed quarantine must not ACK original")
				}
			} else if err != nil || ack.acks != 1 {
				t.Fatal("confirmed quarantine must ACK original")
			}
		})
	}
}
