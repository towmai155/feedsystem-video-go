package rabbitmq

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"feedsystem_video_go/internal/config"
	"log"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

type RabbitMQ struct {
	Conn *amqp.Connection
	Ch   *amqp.Channel

	// Keep publication ordering on a dedicated publishing channel.
	pubMu sync.Mutex
	pubCh *amqp.Channel
}

func NewRabbitMQ(cfg *config.RabbitMQConfig) (*RabbitMQ, error) {
	if cfg == nil {
		return nil, errors.New("rabbitmq config is nil")
	}
	url := (amqp.URI{Scheme: "amqp", Host: cfg.Host, Port: cfg.Port, Username: cfg.Username, Password: cfg.Password, Vhost: "/"}).String()
	conn, err := amqp.DialConfig(url, amqp.Config{Heartbeat: 10 * time.Second, Dial: amqp.DefaultDial(3 * time.Second)})
	if err != nil {
		return nil, err
	}
	ch, err := conn.Channel()
	if err != nil {
		_ = conn.CloseDeadline(time.Now().Add(time.Second))
		return nil, err
	}
	return &RabbitMQ{Conn: conn, Ch: ch}, nil
}

func (r *RabbitMQ) Close() error {
	if r == nil {
		return nil
	}
	var closeErrors []error
	// pubCh is lazily created and dropped under pubMu by PublishJSON; closing it
	// must take the same lock or the shutdown races an in-flight publish.
	r.pubMu.Lock()
	if r.pubCh != nil {
		if err := r.pubCh.Close(); err != nil && !errors.Is(err, amqp.ErrClosed) {
			closeErrors = append(closeErrors, err)
		}
		r.pubCh = nil
	}
	r.pubMu.Unlock()
	if r.Ch != nil {
		if err := r.Ch.Close(); err != nil && !errors.Is(err, amqp.ErrClosed) {
			closeErrors = append(closeErrors, err)
		}
	}
	// A closed channel must not prevent the underlying connection being closed.
	if r.Conn != nil {
		if err := r.Conn.CloseDeadline(time.Now().Add(time.Second)); err != nil && !errors.Is(err, amqp.ErrClosed) {
			closeErrors = append(closeErrors, err)
		}
	}
	return errors.Join(closeErrors...)
}

// NewChannel opens a channel with independently scoped delivery tags and QoS.
func (r *RabbitMQ) NewChannel() (*amqp.Channel, error) {
	if r == nil || r.Conn == nil {
		return nil, errors.New("rabbitmq is not initialized")
	}
	return r.Conn.Channel()
}

// publishChannel returns the shared publish channel, creating it on first use.
// Callers must hold pubMu.
func (r *RabbitMQ) publishChannel() (*amqp.Channel, error) {
	if r.pubCh != nil {
		return r.pubCh, nil
	}
	ch, err := r.Conn.Channel()
	if err != nil {
		return nil, err
	}
	r.pubCh = ch
	return ch, nil
}

func (r *RabbitMQ) DeclareTopic(exchange string, queue string, bindingKey string) error {
	if r == nil || r.Ch == nil {
		return errors.New("rabbitmq is not initialized")
	}
	if exchange == "" || queue == "" || bindingKey == "" {
		return errors.New("exchange/queue/bindingKey is required")
	}

	if err := r.Ch.ExchangeDeclare(
		exchange,
		"topic",
		true,
		false,
		false,
		false,
		nil,
	); err != nil {
		return err
	}

	q, err := r.Ch.QueueDeclare(
		queue,
		true,
		false,
		false,
		false,
		amqp.Table{"x-dead-letter-exchange": DLXExchange},
	)
	if err != nil {
		return err
	}

	if err := r.Ch.QueueBind(
		q.Name,
		bindingKey,
		exchange,
		false,
		nil,
	); err != nil {
		return err
	}
	if err := DeclareDLX(r.Ch, queue); err != nil {
		log.Printf("DLX declare failed for %s: %v", queue, err)
	}
	return nil
}

func (r *RabbitMQ) PublishJSON(ctx context.Context, exchange string, routingKey string, payload any) error {
	if r == nil || r.Ch == nil {
		return errors.New("rabbitmq is not initialized")
	}
	if exchange == "" || routingKey == "" {
		return errors.New("exchange and routingKey are required")
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	r.pubMu.Lock()
	defer r.pubMu.Unlock()
	ch, err := r.publishChannel()
	if err != nil {
		return err
	}
	if err := ch.PublishWithContext(ctx, exchange, routingKey, false, false, amqp.Publishing{
		ContentType:  "application/json",
		DeliveryMode: amqp.Persistent,
		Timestamp:    time.Now(),
		Body:         b,
	}); err != nil {
		// A closed or broken channel poisons every later publish; drop it so the
		// next call reopens one instead of failing forever.
		_ = ch.Close()
		r.pubCh = nil
		return err
	}
	return nil
}

func newEventID(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
