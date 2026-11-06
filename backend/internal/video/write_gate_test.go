package video

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestVideoWriteGateBoundsWritersAndReleasesEntries(t *testing.T) {
	gate := newVideoWriteGate(globalWriteConcurrency)
	var wg sync.WaitGroup
	var active atomic.Int32
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			release, err := gate.acquire(ctx, 7)
			if err != nil {
				t.Error(err)
				return
			}
			defer release()
			if n := active.Add(1); int(n) > perVideoWriteConcurrency {
				t.Errorf("too many same-video writers: %d", n)
			}
			time.Sleep(time.Millisecond)
			active.Add(-1)
		}()
	}
	wg.Wait()
	if len(gate.entries) != 0 {
		t.Fatal("idle video entries leaked")
	}
}

func TestVideoWriteGateCancellationAndIndependentVideos(t *testing.T) {
	gate := newVideoWriteGate(perVideoWriteConcurrency * 2)
	releases := make([]func(), 0, perVideoWriteConcurrency)
	for i := 0; i < perVideoWriteConcurrency; i++ {
		release, err := gate.acquire(context.Background(), 7)
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	other, err := gate.acquire(ctx, 8)
	if err != nil {
		t.Fatalf("unrelated video blocked: %v", err)
	}
	other()
	if _, err := gate.acquire(ctx, 7); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting writer must respect cancellation: %v", err)
	}
	for _, release := range releases {
		release()
	}
	if len(gate.entries) != 0 {
		t.Fatal("canceled writer leaked entry")
	}
}

func TestVideoWriteGateCapsTotalWritersAcrossVideos(t *testing.T) {
	gate := newVideoWriteGate(2)
	first, err := gate.acquire(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	second, err := gate.acquire(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := gate.acquire(ctx, 3); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("global cap must reject the third writer: %v", err)
	}
	// The waiting writer must not leave a per-video entry behind.
	if len(gate.entries) != 2 {
		t.Fatalf("canceled writer leaked entry: %d entries", len(gate.entries))
	}
	first()
	second()
	release, err := gate.acquire(context.Background(), 3)
	if err != nil {
		t.Fatalf("slot was not returned: %v", err)
	}
	release()
	if len(gate.entries) != 0 {
		t.Fatal("idle video entries leaked")
	}
}
