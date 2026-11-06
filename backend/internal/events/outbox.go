// Package events stores messages in the same transaction as business writes.
package events

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type OutboxEvent struct {
	ID            string     `gorm:"type:char(32);primaryKey"`
	Exchange      string     `gorm:"type:varchar(128);not null"`
	RoutingKey    string     `gorm:"type:varchar(128);not null"`
	Payload       string     `gorm:"type:json;not null"`
	CreatedAt     time.Time  `gorm:"not null"`
	PublishedAt   *time.Time `gorm:"index:idx_outbox_due,priority:1"`
	NextAttemptAt time.Time  `gorm:"not null;index:idx_outbox_due,priority:2"`
	Attempts      int        `gorm:"not null;default:0"`
	LeaseToken    string     `gorm:"type:char(32)"`
	LeaseUntil    *time.Time
	LastError     string `gorm:"type:text"`
}

func NewID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func Enqueue(tx *gorm.DB, id, exchange, routingKey string, payload any) error {
	event, err := Build(id, exchange, routingKey, payload)
	if err != nil {
		return err
	}
	return tx.Create(&event).Error
}

func Build(id, exchange, routingKey string, payload any) (OutboxEvent, error) {
	if len(id) != 32 || exchange == "" || routingKey == "" {
		return OutboxEvent{}, fmt.Errorf("invalid outbox event")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return OutboxEvent{}, err
	}
	now := time.Now()
	return OutboxEvent{ID: id, Exchange: exchange, RoutingKey: routingKey, Payload: string(body), CreatedAt: now, NextAttemptAt: now}, nil
}

type Publisher interface {
	Publish(context.Context, string, string, amqp.Publishing) error
}

type Dispatcher struct {
	DB        *gorm.DB
	Publisher Publisher
}

// Claim only holds SQL locks while assigning a lease. Network I/O occurs after
// commit; expired leases recover work after crashes. A publish/mark crash window
// can redeliver, so consumers must deduplicate using the immutable event ID.
func (d *Dispatcher) claim(ctx context.Context) ([]OutboxEvent, error) {
	token, err := NewID()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	until := now.Add(2 * time.Minute)
	var rows []OutboxEvent
	err = d.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where("published_at IS NULL AND next_attempt_at <= ? AND (lease_until IS NULL OR lease_until <= ?)", now, now).
			Order("next_attempt_at, id").Limit(20).Find(&rows).Error; err != nil {
			return err
		}
		ids := make([]string, 0, len(rows))
		for i := range rows {
			rows[i].LeaseToken = token
			ids = append(ids, rows[i].ID)
		}
		if len(ids) > 0 {
			return tx.Model(&OutboxEvent{}).Where("id IN ?", ids).Updates(map[string]any{"lease_token": token, "lease_until": until}).Error
		}
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	return rows, err
}

func (d *Dispatcher) DispatchOnce(ctx context.Context) (int, error) {
	rows, err := d.claim(ctx)
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}
	type failure struct {
		row     OutboxEvent
		updates map[string]any
	}
	succeeded := make([]string, 0, len(rows))
	var failures []failure
	for _, row := range rows {
		publishCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := d.Publisher.Publish(publishCtx, row.Exchange, row.RoutingKey, amqp.Publishing{
			MessageId: row.ID, ContentType: "application/json", DeliveryMode: amqp.Persistent, Timestamp: row.CreatedAt, Body: []byte(row.Payload),
		})
		cancel()
		updates := map[string]any{"lease_token": "", "lease_until": nil}
		if err == nil {
			succeeded = append(succeeded, row.ID)
		} else {
			shift := min(row.Attempts, 5)
			updates["attempts"] = row.Attempts + 1
			updates["next_attempt_at"] = time.Now().Add(time.Duration(1<<shift) * time.Second)
			updates["last_error"] = err.Error()
			failures = append(failures, failure{row: row, updates: updates})
		}
	}
	// Batch the completion markers in a short transaction AFTER network I/O.
	// This avoids a separate disk commit per message. A crash can replay at most
	// the leased batch, with unchanged IDs; consumers deduplicate those deliveries.
	err = d.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if len(succeeded) > 0 {
			if err := tx.Model(&OutboxEvent{}).Where("id IN ? AND lease_token = ?", succeeded, rows[0].LeaseToken).
				Updates(map[string]any{"published_at": time.Now(), "lease_token": "", "lease_until": nil, "last_error": ""}).Error; err != nil {
				return err
			}
		}
		for _, f := range failures {
			if err := tx.Model(&OutboxEvent{}).Where("id = ? AND lease_token = ?", f.row.ID, f.row.LeaseToken).Updates(f.updates).Error; err != nil {
				return err
			}
		}
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	return len(rows), err
}

func (d *Dispatcher) Run(ctx context.Context) {
	lastPrune := time.Now()
	for ctx.Err() == nil {
		n, err := d.DispatchOnce(ctx)
		if err != nil && ctx.Err() == nil {
			log.Printf("interaction outbox: %v", err)
		}
		if time.Since(lastPrune) >= outboxPruneEvery {
			lastPrune = time.Now()
			removed, pruneErr := d.Prune(ctx, outboxRetention)
			if pruneErr != nil {
				if ctx.Err() == nil {
					log.Printf("interaction outbox prune: %v", pruneErr)
				}
			} else if removed > 0 {
				log.Printf("interaction outbox: pruned %d published events", removed)
			}
		}
		if n == 0 || err != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(200 * time.Millisecond):
			}
		}
	}
}

const (
	// Published rows are kept for a few days so a publish/mark mismatch can still
	// be investigated; after that they only consume disk and index space.
	outboxRetention  = 7 * 24 * time.Hour
	outboxPruneEvery = time.Hour
	pruneBatchSize   = 500
	pruneMaxBatches  = 20
)

// Prune removes published events older than retention in small batches so it
// never takes a long lock on the hot table. Safe to run from several processes.
func (d *Dispatcher) Prune(ctx context.Context, retention time.Duration) (int64, error) {
	cutoff := time.Now().Add(-retention)
	var removed int64
	for i := 0; i < pruneMaxBatches; i++ {
		res := d.DB.WithContext(ctx).Limit(pruneBatchSize).
			Where("published_at IS NOT NULL AND published_at < ?", cutoff).
			Delete(&OutboxEvent{})
		if res.Error != nil {
			return removed, res.Error
		}
		removed += res.RowsAffected
		if res.RowsAffected < pruneBatchSize {
			break
		}
	}
	return removed, nil
}
