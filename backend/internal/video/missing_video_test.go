package video

import (
	"context"
	"testing"

	"feedsystem_video_go/internal/events"
	"feedsystem_video_go/internal/testutil"
)

// 互动写入不再单独 SELECT 视频，而是用计数更新的影响行数判断 404：
// 视频不存在时必须整体回滚，不能留下点赞行、评论或事件。
func TestInteractionOnMissingVideoLeavesNoTrace(t *testing.T) {
	db := testutil.NewMySQL(t)
	if err := db.AutoMigrate(&Video{}, &Like{}, &Comment{}, &events.OutboxEvent{}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const missingVideo = 987654

	likeService := NewLikeService(NewLikeRepository(db), NewVideoRepository(db), nil, nil, nil)
	if err := likeService.Like(ctx, &Like{VideoID: missingVideo, AccountID: 3}); err == nil || err.Error() != "video not found" {
		t.Fatalf("like on missing video: %v", err)
	}
	commentService := NewCommentService(NewCommentRepository(db), NewVideoRepository(db), nil, nil, nil)
	if err := commentService.Publish(ctx, &Comment{VideoID: missingVideo, AuthorID: 3, Username: "u", Content: "test"}); err == nil || err.Error() != "video not found" {
		t.Fatalf("comment on missing video: %v", err)
	}

	var likes, comments, outbox int64
	db.Model(&Like{}).Count(&likes)
	db.Model(&Comment{}).Count(&comments)
	db.Model(&events.OutboxEvent{}).Count(&outbox)
	if likes != 0 || comments != 0 || outbox != 0 {
		t.Fatalf("missing video left traces: likes=%d comments=%d outbox=%d", likes, comments, outbox)
	}
}
