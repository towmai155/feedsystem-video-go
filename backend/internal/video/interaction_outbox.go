package video

import (
	"time"

	"feedsystem_video_go/internal/events"
	"feedsystem_video_go/internal/middleware/rabbitmq"
	"gorm.io/gorm"
)

func enqueueInteraction(tx *gorm.DB, id, exchange, key string, payload any, videoID uint, delta int64) error {
	first, err := events.Build(id, exchange, key, payload)
	if err != nil {
		return err
	}
	popularityID, err := events.NewID()
	if err != nil {
		return err
	}
	second, err := events.Build(popularityID, "video.popularity.events", "video.popularity.update", rabbitmq.PopularityEvent{
		EventID: popularityID, VideoID: videoID, Change: delta, OccurredAt: time.Now().UTC(),
	})
	if err != nil {
		return err
	}
	return tx.Create(&[]events.OutboxEvent{first, second}).Error
}

func enqueueLike(tx *gorm.DB, like *Like, action string, delta int64) error {
	id, err := events.NewID()
	if err != nil {
		return err
	}
	return enqueueInteraction(tx, id, "like.events", "like."+action, rabbitmq.LikeEvent{
		EventID: id, Action: action, UserID: like.AccountID, VideoID: like.VideoID, OccurredAt: time.Now().UTC(), Applied: true,
	}, like.VideoID, delta)
}

func enqueueComment(tx *gorm.DB, comment *Comment) error {
	id, err := events.NewID()
	if err != nil {
		return err
	}
	return enqueueInteraction(tx, id, "comment.events", "comment.publish", rabbitmq.CommentEvent{
		EventID: id, Action: "publish", CommentID: comment.ID, Username: comment.Username, VideoID: comment.VideoID,
		AuthorID: comment.AuthorID, Content: comment.Content, OccurredAt: time.Now().UTC(), Applied: true,
	}, comment.VideoID, 1)
}
