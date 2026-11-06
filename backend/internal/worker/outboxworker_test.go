package worker

import (
	"sync"
	"testing"
	"time"

	"feedsystem_video_go/internal/testutil"
	"feedsystem_video_go/internal/video"
)

// Two pollers must never publish the same timeline row: only one conditional
// update can move it out of "pending".
func TestTimelineOutboxClaimIsExclusive(t *testing.T) {
	db := testutil.NewMySQL(t)
	if err := db.AutoMigrate(&video.OutboxMsg{}); err != nil {
		t.Fatal(err)
	}
	msg := video.OutboxMsg{VideoID: 7, EventType: "publish", Status: "pending"}
	if err := db.Create(&msg).Error; err != nil {
		t.Fatal(err)
	}

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		wins  int
		wgErr error
	)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := claimTimelineOutbox(db, msg.ID)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				wgErr = err
				return
			}
			if ok {
				wins++
			}
		}()
	}
	wg.Wait()
	if wgErr != nil {
		t.Fatal(wgErr)
	}
	if wins != 1 {
		t.Fatalf("claim must be exclusive, got %d winners", wins)
	}
}

func TestStaleTimelineClaimReturnsToPending(t *testing.T) {
	db := testutil.NewMySQL(t)
	if err := db.AutoMigrate(&video.OutboxMsg{}); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-time.Hour)
	fresh := time.Now()
	msgs := []video.OutboxMsg{
		{VideoID: 1, EventType: "publish", Status: "sending", ClaimedAt: &stale},
		{VideoID: 2, EventType: "publish", Status: "sending", ClaimedAt: &fresh},
	}
	if err := db.Create(&msgs).Error; err != nil {
		t.Fatal(err)
	}

	if err := releaseStaleTimelineOutbox(db); err != nil {
		t.Fatal(err)
	}

	var staleRow, freshRow video.OutboxMsg
	if err := db.First(&staleRow, msgs[0].ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&freshRow, msgs[1].ID).Error; err != nil {
		t.Fatal(err)
	}
	if staleRow.Status != "pending" || staleRow.ClaimedAt != nil {
		t.Fatalf("stale claim must be released: %+v", staleRow)
	}
	if freshRow.Status != "sending" {
		t.Fatalf("a live claim must be left alone: %+v", freshRow)
	}
}
