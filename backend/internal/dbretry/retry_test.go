package dbretry

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

func TestRunRetriesTransientLockConflicts(t *testing.T) {
	for _, number := range []uint16{1213, 1205} {
		attempts := 0
		err := Run(context.Background(), func() error {
			attempts++
			if attempts < 3 {
				return &mysql.MySQLError{Number: number, Message: "lock conflict"}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("%d: unexpected error: %v", number, err)
		}
		if attempts != 3 {
			t.Fatalf("%d: expected 3 attempts, got %d", number, attempts)
		}
	}
}

func TestRunGivesUpAfterDefaultAttempts(t *testing.T) {
	attempts := 0
	err := Run(context.Background(), func() error {
		attempts++
		return &mysql.MySQLError{Number: 1213, Message: "deadlock"}
	})
	if !Transient(err) {
		t.Fatalf("expected the deadlock error to be returned, got %v", err)
	}
	if attempts != DefaultAttempts {
		t.Fatalf("expected %d attempts, got %d", DefaultAttempts, attempts)
	}
}

func TestRunDoesNotRetryBusinessErrors(t *testing.T) {
	sentinel := errors.New("video not found")
	attempts := 0
	err := Run(context.Background(), func() error {
		attempts++
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected the original error, got %v", err)
	}
	if attempts != 1 {
		t.Fatalf("business error must not be retried, got %d attempts", attempts)
	}
}

func TestRunStopsWhenContextIsCanceled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Millisecond)
	defer cancel()
	attempts := 0
	err := Run(ctx, func() error {
		attempts++
		return &mysql.MySQLError{Number: 1213, Message: "deadlock"}
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected the context error, got %v", err)
	}
	if attempts != 1 {
		t.Fatalf("expected a single attempt before giving up, got %d", attempts)
	}
}
