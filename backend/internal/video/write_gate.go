package video

import (
	"context"
	"log"
	"os"
	"strconv"
	"sync"
)

// Queue same-video writes before they borrow a SQL connection. The database row
// already serializes these transactions; waiting there exhausts the shared pool
// and blocks unrelated session lookups. SQL transactions and unique indexes
// still enforce correctness across API instances.
//
// The size of the per-video window is what makes throughput scale. Every
// durable MySQL commit waits for its own fsync, and group commit only amortizes
// that wait when several commits are in flight at the same time; a window of 4
// left the database mostly idle. The global cap then keeps the number of write
// transactions below the connection pool so authentication and reads keep
// their connections.
type videoWriteGate struct {
	global   chan struct{}
	perVideo int
	mu       sync.Mutex
	entries  map[uint]*videoWriteEntry
}

type videoWriteEntry struct {
	token chan struct{}
	users int
}

var (
	// Both limits are per API process and can be tuned without a rebuild.
	// WRITE_GATE_GLOBAL should stay below MYSQL_MAX_OPEN_CONNS so that a write
	// burst cannot starve session lookups; WRITE_GATE_PER_VIDEO only bounds how
	// many transactions target the same hot video.
	perVideoWriteConcurrency = intFromEnv("WRITE_GATE_PER_VIDEO", 16)
	globalWriteConcurrency   = intFromEnv("WRITE_GATE_GLOBAL", 32)

	interactionWrites = newVideoWriteGate(globalWriteConcurrency, perVideoWriteConcurrency)
)

func intFromEnv(name string, fallback int) int {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		log.Printf("write gate: ignoring invalid %s=%q, using %d", name, raw, fallback)
		return fallback
	}
	return value
}

func newVideoWriteGate(global, perVideo int) videoWriteGate {
	return videoWriteGate{
		global:   make(chan struct{}, global),
		perVideo: perVideo,
		entries:  make(map[uint]*videoWriteEntry),
	}
}

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
		e = &videoWriteEntry{token: make(chan struct{}, g.perVideo)}
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
	// Take the per-video slot first: a hot video may only occupy its own
	// window, not the whole global budget, and every writer takes both slots in
	// the same order so the two limits cannot deadlock each other.
	select {
	case e.token <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-e.token
			forget()
			return nil, err
		}
	case <-ctx.Done():
		forget()
		return nil, ctx.Err()
	}
	select {
	case g.global <- struct{}{}:
		return func() {
			<-g.global
			<-e.token
			forget()
		}, nil
	case <-ctx.Done():
		<-e.token
		forget()
		return nil, ctx.Err()
	}
}
