package video

import (
	"context"
	"errors"
	"testing"

	"feedsystem_video_go/internal/events"
	"feedsystem_video_go/internal/testutil"
)

func TestInteractionCommitIncludesEventsWithoutBroker(t *testing.T) {
	db := testutil.NewMySQL(t)
	if err := db.AutoMigrate(&Video{}, &Like{}, &Comment{}, &events.OutboxEvent{}); err != nil {
		t.Fatal(err)
	}
	v := Video{AuthorID: 9, Username: "author", Title: "test", PlayURL: "test.mp4", CoverURL: "test.jpg"}
	if err := db.Create(&v).Error; err != nil {
		t.Fatal(err)
	}
	s := NewLikeService(NewLikeRepository(db), NewVideoRepository(db), nil, nil, nil)
	ctx := context.Background()
	if err := s.Like(ctx, &Like{VideoID: v.ID, AccountID: 3}); err != nil {
		t.Fatal(err)
	}
	if err := s.Like(ctx, &Like{VideoID: v.ID, AccountID: 3}); !errors.Is(err, ErrAlreadyLiked) {
		t.Fatalf("duplicate like: %v", err)
	}
	var count int64
	db.Model(&events.OutboxEvent{}).Count(&count)
	if count != 2 {
		t.Fatalf("duplicate request created duplicate events: %d", count)
	}
	c := NewCommentService(NewCommentRepository(db), NewVideoRepository(db), nil, nil, nil)
	if err := c.Publish(ctx, &Comment{VideoID: v.ID, AuthorID: 3, Username: "user", Content: "test"}); err != nil {
		t.Fatal(err)
	}
	db.Model(&events.OutboxEvent{}).Where("published_at IS NULL").Count(&count)
	if count != 4 {
		t.Fatalf("expected 4 durable pending events without MQ, got %d", count)
	}
	if err := s.Unlike(ctx, &Like{VideoID: v.ID, AccountID: 3}); err != nil {
		t.Fatal(err)
	}
	db.Model(&events.OutboxEvent{}).Count(&count)
	if count != 6 {
		t.Fatalf("unlike events missing: %d", count)
	}
}

func TestInteractionRollbackWhenEventCannotBeStored(t *testing.T) {
	for _, kind := range []string{"like", "comment"} {
		t.Run(kind, func(t *testing.T) {
			db := testutil.NewMySQL(t)
			// Deliberately omit the outbox table to exercise an event-write failure.
			if err := db.AutoMigrate(&Video{}, &Like{}, &Comment{}); err != nil {
				t.Fatal(err)
			}
			v := Video{AuthorID: 9, Username: "author", Title: "test", PlayURL: "test.mp4", CoverURL: "test.jpg"}
			if err := db.Create(&v).Error; err != nil {
				t.Fatal(err)
			}
			var err error
			if kind == "like" {
				err = NewLikeService(NewLikeRepository(db), NewVideoRepository(db), nil, nil, nil).Like(context.Background(), &Like{VideoID: v.ID, AccountID: 3})
			} else {
				err = NewCommentService(NewCommentRepository(db), NewVideoRepository(db), nil, nil, nil).Publish(context.Background(), &Comment{VideoID: v.ID, AuthorID: 3, Username: "u", Content: "test"})
			}
			if err == nil {
				t.Fatal("missing event table should fail transaction")
			}
			var likes, comments int64
			db.Model(&Like{}).Count(&likes)
			db.Model(&Comment{}).Count(&comments)
			db.First(&v, v.ID)
			if likes != 0 || comments != 0 || v.LikesCount != 0 || v.Popularity != 0 {
				t.Fatalf("partial commit: likes=%d comments=%d video=%+v", likes, comments, v)
			}
		})
	}
}
