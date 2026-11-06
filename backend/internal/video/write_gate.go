package video

import (
	"context"
	"sync"
)

// Queue same-video writes before they borrow a SQL connection. The database row
// already serializes these transactions; waiting there exhausts the shared pool
// and blocks unrelated session lookups. SQL transactions and unique indexes
// still enforce correctness across API instances.
type videoWriteGate struct {
	mu      sync.Mutex
	entries map[uint]*videoWriteEntry
}

type videoWriteEntry struct {
	token chan struct{}
	users int
}

var interactionWrites videoWriteGate

// A small amount of parallelism overlaps validation, inserts and event
// publication while leaving connections available for authentication/readers.
const perVideoWriteConcurrency = 4

func (g *videoWriteGate) acquire(ctx context.Context, id uint) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	g.mu.Lock()
	if g.entries == nil {
		g.entries = make(map[uint]*videoWriteEntry)
	}
	e := g.entries[id]
	if e == nil {
		e = &videoWriteEntry{token: make(chan struct{}, perVideoWriteConcurrency)}
		g.entries[id] = e
	}
	e.users++
	g.mu.Unlock()
	forget := func() {
		g.mu.Lock()
		e.users--
		if e.users == 0 {
			delete(g.entries, id)
		}
		g.mu.Unlock()
	}
	select {
	case e.token <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-e.token
			forget()
			return nil, err
		}
		return func() { <-e.token; forget() }, nil
	case <-ctx.Done():
		forget()
		return nil, ctx.Err()
	}
}
