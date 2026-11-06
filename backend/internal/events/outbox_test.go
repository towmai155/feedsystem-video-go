package events

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"feedsystem_video_go/internal/testutil"
	amqp "github.com/rabbitmq/amqp091-go"
	"gorm.io/gorm"
)

type testPublisher struct {
	fail bool
	ids  []string
}

func (p *testPublisher) Publish(_ context.Context, _, _ string, msg amqp.Publishing) error {
	p.ids = append(p.ids, msg.MessageId)
	if p.fail {
		return errors.New("broker unavailable")
	}
	return nil
}

func TestOutboxRollbackAndRetryPreservesIdentity(t *testing.T) {
	db := testutil.NewMySQL(t)
	if err := db.AutoMigrate(&OutboxEvent{}); err != nil {
		t.Fatal(err)
	}
	id, _ := NewID()
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := Enqueue(tx, id, "test.events", "test.created", map[string]string{"event_id": id}); err != nil {
			return err
		}
		return errors.New("business transaction rolled back")
	})
	if err == nil {
		t.Fatal("expected rollback")
	}
	var count int64
	db.Model(&OutboxEvent{}).Count(&count)
	if count != 0 {
		t.Fatal("rolled-back event survived")
	}
	if err := Enqueue(db, id, "test.events", "test.created", map[string]string{"event_id": id}); err != nil {
		t.Fatal(err)
	}
	publisher := &testPublisher{fail: true}
	dispatcher := Dispatcher{DB: db, Publisher: publisher}
	if _, err := dispatcher.DispatchOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	var row OutboxEvent
	if err := db.First(&row, "id = ?", id).Error; err != nil {
		t.Fatal(err)
	}
	if row.PublishedAt != nil || row.Attempts != 1 || row.LastError == "" {
		t.Fatalf("failed publication not retained: %+v", row)
	}
	publisher.fail = false
	db.Model(&row).Update("next_attempt_at", time.Now().Add(-time.Second))
	if _, err := dispatcher.DispatchOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	row = OutboxEvent{}
	db.First(&row, "id = ?", id)
	if row.PublishedAt == nil || len(publisher.ids) != 2 || publisher.ids[0] != publisher.ids[1] {
		t.Fatalf("retry lost identity or event: %+v %v", row, publisher.ids)
	}
}

func TestConcurrentClaimsAndExpiredLeaseRecovery(t *testing.T) {
	db := testutil.NewMySQL(t)
	if err := db.AutoMigrate(&OutboxEvent{}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		id, _ := NewID()
		if err := Enqueue(db, id, "test", "test", map[string]int{"i": i}); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	rows := make(chan []OutboxEvent, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d := Dispatcher{DB: db}
			claimed, err := d.claim(context.Background())
			if err != nil {
				t.Error(err)
			}
			rows <- claimed
		}()
	}
	wg.Wait()
	close(rows)
	seen := map[string]bool{}
	for batch := range rows {
		for _, row := range batch {
			if seen[row.ID] {
				t.Fatal("overlapping active leases")
			}
			seen[row.ID] = true
		}
	}
	d := Dispatcher{DB: db}
	// A concurrent locking scan can temporarily skip additional rows; the next
	// poll must claim remaining work without taking any already-active lease.
	if len(seen) < 30 {
		remaining, err := d.claim(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range remaining {
			if seen[row.ID] {
				t.Fatal("active lease claimed twice")
			}
			seen[row.ID] = true
		}
	}
	if len(seen) != 30 {
		t.Fatalf("claimed %d/30 events", len(seen))
	}
	claimed, err := d.claim(context.Background())
	if err != nil || len(claimed) != 0 {
		t.Fatalf("active leases reclaimed: %v", err)
	}
	if err := db.Model(&OutboxEvent{}).Where("published_at IS NULL").Update("lease_until", time.Now().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	claimed, err = d.claim(context.Background())
	if err != nil || len(claimed) != 20 {
		t.Fatalf("expired leases not recovered: %d %v", len(claimed), err)
	}
}

func TestConfirmedPublicationWithFailedCompletionMarkReplaysSameID(t *testing.T) {
	db := testutil.NewMySQL(t)
	if err := db.AutoMigrate(&OutboxEvent{}); err != nil {
		t.Fatal(err)
	}
	id, _ := NewID()
	if err := Enqueue(db, id, "test", "test", map[string]string{"event_id": id}); err != nil {
		t.Fatal(err)
	}
	publisher := &testPublisher{}
	d := Dispatcher{DB: db, Publisher: publisher}
	if err := db.Callback().Update().Before("gorm:update").Register("test_fail_completion", func(tx *gorm.DB) {
		if updates, ok := tx.Statement.Dest.(map[string]any); ok {
			if _, completion := updates["published_at"]; completion {
				tx.AddError(errors.New("lost completion write"))
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.DispatchOnce(context.Background()); err == nil {
		t.Fatal("completion failure hidden")
	}
	db.Callback().Update().Remove("test_fail_completion")
	var row OutboxEvent
	if err := db.First(&row, "id = ?", id).Error; err != nil {
		t.Fatal(err)
	}
	if row.PublishedAt != nil {
		t.Fatal("failed completion marked published")
	}
	if err := db.Model(&row).Update("lease_until", time.Now().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := d.DispatchOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(publisher.ids) != 2 || publisher.ids[0] != publisher.ids[1] {
		t.Fatalf("recovery changed identity: %v", publisher.ids)
	}
}

// A poller holding its selected rows must not also lock the insert gap used by
// new HTTP transactions. READ COMMITTED avoids those RR next-key/gap locks.
func TestClaimDoesNotBlockNewBusinessEvents(t *testing.T) {
	db := testutil.NewMySQL(t)
	if err := db.AutoMigrate(&OutboxEvent{}); err != nil {
		t.Fatal(err)
	}
	id, _ := NewID()
	if err := Enqueue(db, id, "test", "test", map[string]int{"i": 1}); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	if err := db.Callback().Query().After("gorm:query").Register("pause_outbox_claim", func(tx *gorm.DB) {
		if _, locking := tx.Statement.Clauses["FOR"]; locking && tx.Statement.Table == "outbox_events" {
			close(entered)
			<-release
		}
	}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := (&Dispatcher{DB: db}).claim(context.Background()); done <- err }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("claim did not reach locking read")
	}
	insertCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	id, _ = NewID()
	insertErr := Enqueue(db.WithContext(insertCtx), id, "test", "test", map[string]int{"i": 2})
	close(release)
	claimErr := <-done
	if insertErr != nil {
		t.Fatalf("claim blocked a new event insertion: %v", insertErr)
	}
	if claimErr != nil {
		t.Fatal(claimErr)
	}
}
