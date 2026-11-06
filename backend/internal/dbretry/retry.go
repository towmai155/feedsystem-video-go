// Package dbretry re-runs transactions that MySQL aborted because of a lock
// conflict. InnoDB reports 1213 (deadlock) and 1205 (lock wait timeout) for
// errors that disappear when the same transaction is simply executed again,
// and both become expected as soon as writes to one video row really run in
// parallel.
package dbretry

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/go-sql-driver/mysql"
)

// DefaultAttempts is the total number of tries, so 3 means the first attempt
// plus two retries.
const DefaultAttempts = 3

// Transient reports whether err is a lock conflict that is worth retrying.
func Transient(err error) bool {
	var me *mysql.MySQLError
	if !errors.As(err, &me) {
		return false
	}
	return me.Number == 1213 || me.Number == 1205
}

// Run calls fn and repeats it while it fails with a transient lock error.
//
// fn must be safe to run more than once: a transaction aborted by InnoDB
// leaves nothing behind, but values written into its arguments (auto-increment
// ids, timestamps assigned by the database) have to be reset by the caller.
func Run(ctx context.Context, fn func() error) error {
	var err error
	for attempt := 1; attempt <= DefaultAttempts; attempt++ {
		err = fn()
		if err == nil || !Transient(err) {
			return err
		}
		if attempt == DefaultAttempts {
			break
		}
		// Jitter keeps the retries of concurrent transactions from colliding
		// again in lockstep.
		wait := time.Duration(attempt)*5*time.Millisecond + time.Duration(rand.IntN(10))*time.Millisecond
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return err
}
