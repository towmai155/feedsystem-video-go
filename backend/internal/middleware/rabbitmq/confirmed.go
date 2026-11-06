package rabbitmq

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"feedsystem_video_go/internal/config"
	amqp "github.com/rabbitmq/amqp091-go"
)

// ConfirmedPublisher reconnects after failures and only reports success after a
// broker ACK. One in-flight publish makes mandatory returns unambiguous.
type ConfirmedPublisher struct {
	cfg     config.RabbitMQConfig
	mu      sync.Mutex
	conn    *amqp.Connection
	socket  net.Conn
	ch      *amqp.Channel
	returns <-chan amqp.Return
}

func NewConfirmedPublisher(cfg config.RabbitMQConfig) *ConfirmedPublisher {
	return &ConfirmedPublisher{cfg: cfg}
}

func (p *ConfirmedPublisher) connect(ctx context.Context) error {
	if p.conn != nil && !p.conn.IsClosed() {
		return nil
	}
	url := (amqp.URI{Scheme: "amqp", Host: p.cfg.Host, Port: p.cfg.Port, Username: p.cfg.Username, Password: p.cfg.Password, Vhost: "/"}).String()
	conn, err := amqp.DialConfig(url, amqp.Config{Heartbeat: 10 * time.Second, Dial: func(network, address string) (net.Conn, error) {
		socket, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, network, address)
		if err == nil {
			p.socket = socket
			_ = socket.SetDeadline(time.Now().Add(5 * time.Second))
		}
		return socket, err
	}})
	if err != nil {
		if p.socket != nil {
			_ = p.socket.Close()
		}
		return err
	}
	p.conn = conn
	ch, err := conn.Channel()
	if err != nil {
		p.reset()
		return err
	}
	p.ch = ch
	if err := DeclareReliableTopology(ch); err != nil {
		p.reset()
		return err
	}
	if err := ch.Confirm(false); err != nil {
		p.reset()
		return err
	}
	p.returns = ch.NotifyReturn(make(chan amqp.Return, 1))
	_ = p.socket.SetDeadline(time.Time{})
	return nil
}

func (p *ConfirmedPublisher) reset() {
	if p.conn != nil {
		_ = p.conn.CloseDeadline(time.Now().Add(time.Second))
	}
	if p.socket != nil {
		_ = p.socket.Close()
	}
	p.conn = nil
	p.socket = nil
	p.ch = nil
	p.returns = nil
}

func (p *ConfirmedPublisher) Close() error { p.mu.Lock(); defer p.mu.Unlock(); p.reset(); return nil }

func (p *ConfirmedPublisher) Publish(ctx context.Context, exchange, key string, msg amqp.Publishing) (err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = p.connect(ctx); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			p.reset()
		} else {
			_ = p.socket.SetWriteDeadline(time.Time{})
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	if end, ok := ctx.Deadline(); ok && end.Before(deadline) {
		deadline = end
	}
	_ = p.socket.SetWriteDeadline(deadline)
	confirmation, err := p.ch.PublishWithDeferredConfirm(exchange, key, true, false, msg)
	if err != nil {
		return err
	}
	if confirmation == nil {
		return errors.New("publisher confirms unavailable")
	}
	ack, err := confirmation.WaitContext(ctx)
	if err != nil {
		return err
	}
	if !ack {
		return errors.New("broker rejected publication")
	}
	// RabbitMQ sends basic.return before the corresponding publisher confirm.
	select {
	case returned, ok := <-p.returns:
		if !ok {
			return errors.New("publisher return listener closed")
		}
		return fmt.Errorf("unroutable message: %s", returned.ReplyText)
	default:
		return nil
	}
}

// Establish every required binding before marking an outbox event published.
// A mandatory publish ACK alone only proves that at least one queue matched.
func DeclareReliableTopology(ch *amqp.Channel) error {
	bindings := []struct{ exchange, queue, key string }{
		{"like.events", "like.events", "like.*"},
		{"like.events", "notification.like", "like.like"},
		{"comment.events", "comment.events", "comment.*"},
		{"comment.events", "notification.comment", "comment.publish"},
		{"social.events", "social.events", "social.*"},
		{"social.events", "notification.social", "social.follow"},
		{"video.popularity.events", "video.popularity.events", "video.popularity.*"},
	}
	for _, b := range bindings {
		if err := ch.ExchangeDeclare(b.exchange, "topic", true, false, false, false, nil); err != nil {
			return err
		}
		if _, err := ch.QueueDeclare(b.queue, true, false, false, false, amqp.Table{"x-dead-letter-exchange": DLXExchange}); err != nil {
			return err
		}
		if err := ch.QueueBind(b.queue, b.key, b.exchange, false, nil); err != nil {
			return err
		}
		if err := DeclareDLX(ch, b.queue); err != nil {
			return err
		}
	}
	return nil
}
