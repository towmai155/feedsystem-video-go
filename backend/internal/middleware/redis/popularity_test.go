package redis

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	miniredis "github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
)

func TestPopularityRetryIsAtomicAndDeduplicated(t *testing.T) {
	mr := miniredis.RunT(t)
	c := &Client{rdb: goredis.NewClient(&goredis.Options{Addr: mr.Addr()}), keyPrefix: "test:"}
	defer c.Close()
	expires := time.Now().Add(time.Hour)
	ctx := context.Background()
	mr.Set("window", "wrong-type")
	if err := c.ApplyPopularityEvent(ctx, "event-1", "window", "7", "detail", 1, expires); err == nil {
		t.Fatal("wrong type must propagate")
	}
	if mr.Exists(c.Key("processed:popularity:%s", "event-1")) {
		t.Fatal("failed operation was marked applied")
	}
	mr.Del("window")
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.ApplyPopularityEvent(ctx, "event-1", "window", "7", "detail", 1, expires); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	score, err := mr.ZScore("window", "7")
	if err != nil || score != 1 {
		t.Fatalf("redelivery duplicated score: %v %v", score, err)
	}
	if mr.TTL(c.Key("processed:popularity:%s", "event-1")) <= mr.TTL("window") {
		t.Fatal("dedup marker must outlive score window")
	}
}

func TestOldPopularityReplayDoesNotResurrectWindow(t *testing.T) {
	mr := miniredis.RunT(t)
	c := &Client{rdb: goredis.NewClient(&goredis.Options{Addr: mr.Addr()}), keyPrefix: "test:"}
	defer c.Close()
	err := c.ApplyPopularityEvent(context.Background(), "old", "old-window", "7", "detail", 1, time.Now().Add(-time.Hour))
	if !errors.Is(err, ErrPopularityWindowExpired) {
		t.Fatalf("expired window must be reported, not silently dropped: %v", err)
	}
	if mr.Exists("old-window") {
		t.Fatal("expired minute window resurrected")
	}
	if !mr.Exists(c.Key("processed:popularity:%s", "old")) {
		t.Fatal("skipped event must still be marked so it is not retried forever")
	}
	if err := c.ApplyPopularityEvent(context.Background(), "old", "old-window", "7", "detail", 1, time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("redelivery of a skipped event must be a no-op: %v", err)
	}
}
