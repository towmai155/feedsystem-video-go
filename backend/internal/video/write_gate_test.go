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
	var gate videoWriteGate
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
			if n := active.Add(1); n > perVideoWriteConcurrency {
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
	var gate videoWriteGate
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
