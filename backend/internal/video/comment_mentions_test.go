package video

import (
	"context"
	"testing"
	"time"

	"feedsystem_video_go/internal/account"
	"feedsystem_video_go/internal/events"
	"feedsystem_video_go/internal/testutil"
)

// notificationRow mirrors worker.Notification; the video package cannot import
// worker without a cycle, and the table layout is what the atomic write targets.
type notificationRow struct {
	ID          uint `gorm:"primaryKey"`
	RecipientID uint
	SenderID    uint
	Type        string
	TargetID    uint
	Content     string
	IsRead      bool
	CreatedAt   time.Time
	EventID     *string `gorm:"type:varchar(96);uniqueIndex:idx_notification_event"`
}

func (notificationRow) TableName() string { return "notifications" }

func TestCommentMentionNotificationIsWrittenInTheSameTransaction(t *testing.T) {
	db := testutil.NewMySQL(t)
	if err := db.AutoMigrate(&Video{}, &Comment{}, &events.OutboxEvent{}, &account.Account{}, &notificationRow{}); err != nil {
		t.Fatal(err)
	}
	author := &account.Account{Username: "author"}
	mentioned := &account.Account{Username: "bob"}
	if err := db.Create(author).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(mentioned).Error; err != nil {
		t.Fatal(err)
	}
	v := Video{AuthorID: author.ID, Username: "author", Title: "test", PlayURL: "test.mp4", CoverURL: "test.jpg"}
	if err := db.Create(&v).Error; err != nil {
		t.Fatal(err)
	}

	s := NewCommentService(NewCommentRepository(db), NewVideoRepository(db), nil, nil, nil)
	comment := &Comment{VideoID: v.ID, AuthorID: author.ID, Username: author.Username,
		Content: "hello @bob, and again @bob and @author"}
	if err := s.Publish(context.Background(), comment); err != nil {
		t.Fatal(err)
	}

	var rows []notificationRow
	if err := db.Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected one deduplicated mention notification, got %d", len(rows))
	}
	if rows[0].RecipientID != mentioned.ID || rows[0].Type != "mention" || rows[0].TargetID != v.ID {
		t.Fatalf("unexpected mention notification: %+v", rows[0])
	}
	if rows[0].EventID == nil || *rows[0].EventID == "" {
		t.Fatal("mention notification must carry an event id so retries stay idempotent")
	}
}

func TestCommentRollsBackWhenMentionCannotBeStored(t *testing.T) {
	db := testutil.NewMySQL(t)
	// The notifications table is deliberately missing: the mention write fails
	// and the comment must not become visible without its notification.
	if err := db.AutoMigrate(&Video{}, &Comment{}, &events.OutboxEvent{}, &account.Account{}); err != nil {
		t.Fatal(err)
	}
	author := &account.Account{Username: "author"}
	mentioned := &account.Account{Username: "bob"}
	if err := db.Create(author).Error; err != nil {
		t.Fatal(err)
	}
	// The mentioned account must exist, otherwise there is no notification row to
	// insert and the missing table would go unnoticed.
	if err := db.Create(mentioned).Error; err != nil {
		t.Fatal(err)
	}
	v := Video{AuthorID: author.ID, Username: "author", Title: "test", PlayURL: "test.mp4", CoverURL: "test.jpg"}
	if err := db.Create(&v).Error; err != nil {
		t.Fatal(err)
	}

	s := NewCommentService(NewCommentRepository(db), NewVideoRepository(db), nil, nil, nil)
	err := s.Publish(context.Background(), &Comment{VideoID: v.ID, AuthorID: author.ID, Username: author.Username,
		Content: "hello @bob"})
	if err == nil {
		t.Fatal("missing notifications table should fail the transaction")
	}
	var comments, outbox int64
	db.Model(&Comment{}).Count(&comments)
	db.Model(&events.OutboxEvent{}).Count(&outbox)
	if comments != 0 || outbox != 0 {
		t.Fatalf("partial commit: comments=%d outbox=%d", comments, outbox)
	}
}
