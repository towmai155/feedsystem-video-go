package worker

import (
	"context"
	"testing"

	"feedsystem_video_go/internal/middleware/rabbitmq"
	"feedsystem_video_go/internal/testutil"
	"feedsystem_video_go/internal/video"
)

// A legacy comment event has no Applied flag, so the consumer owns the write.
// Redelivering it after a commit must not create a second comment or add the
// popularity twice.
func TestLegacyCommentRedeliveryDoesNotDuplicate(t *testing.T) {
	db := testutil.NewMySQL(t)
	if err := db.AutoMigrate(&video.Video{}, &video.Comment{}); err != nil {
		t.Fatal(err)
	}
	v := video.Video{AuthorID: 1, Username: "author", Title: "t", PlayURL: "p.mp4", CoverURL: "c.jpg"}
	if err := db.Create(&v).Error; err != nil {
		t.Fatal(err)
	}

	w := NewCommentWorker(nil, video.NewCommentRepository(db), video.NewVideoRepository(db), "comment.events")
	evt := &rabbitmq.CommentEvent{
		EventID: "legacy-comment-1", Action: "publish", VideoID: v.ID,
		AuthorID: 2, Username: "bob", Content: "hello",
	}
	for i := 0; i < 3; i++ {
		if err := w.applyPublish(context.Background(), evt); err != nil {
			t.Fatalf("delivery %d: %v", i, err)
		}
	}

	var comments int64
	if err := db.Model(&video.Comment{}).Count(&comments).Error; err != nil {
		t.Fatal(err)
	}
	var stored video.Video
	if err := db.First(&stored, v.ID).Error; err != nil {
		t.Fatal(err)
	}
	if comments != 1 || stored.Popularity != 1 {
		t.Fatalf("redelivery was not idempotent: comments=%d popularity=%d", comments, stored.Popularity)
	}
}

// The legacy like path inserts the row and updates both counters in one
// transaction, so a redelivery cannot increment them again.
func TestLegacyLikeRedeliveryDoesNotRecount(t *testing.T) {
	db := testutil.NewMySQL(t)
	if err := db.AutoMigrate(&video.Video{}, &video.Like{}); err != nil {
		t.Fatal(err)
	}
	v := video.Video{AuthorID: 1, Username: "author", Title: "t", PlayURL: "p.mp4", CoverURL: "c.jpg"}
	if err := db.Create(&v).Error; err != nil {
		t.Fatal(err)
	}

	w := NewLikeWorker(nil, video.NewLikeRepository(db), video.NewVideoRepository(db), "like.events")
	for i := 0; i < 3; i++ {
		if err := w.applyLike(context.Background(), 2, v.ID); err != nil {
			t.Fatalf("delivery %d: %v", i, err)
		}
	}

	var likes int64
	if err := db.Model(&video.Like{}).Count(&likes).Error; err != nil {
		t.Fatal(err)
	}
	var stored video.Video
	if err := db.First(&stored, v.ID).Error; err != nil {
		t.Fatal(err)
	}
	if likes != 1 || stored.LikesCount != 1 || stored.Popularity != 1 {
		t.Fatalf("redelivery was not idempotent: likes=%d likes_count=%d popularity=%d",
			likes, stored.LikesCount, stored.Popularity)
	}

	if err := w.applyUnlike(context.Background(), 2, v.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&stored, v.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.LikesCount != 0 || stored.Popularity != 0 {
		t.Fatalf("unlike did not reverse the counters: likes_count=%d popularity=%d", stored.LikesCount, stored.Popularity)
	}
}
