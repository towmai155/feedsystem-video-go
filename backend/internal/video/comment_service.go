package video

import (
	"context"
	"errors"
	"feedsystem_video_go/internal/apierror"
	"feedsystem_video_go/internal/events"
	"feedsystem_video_go/internal/middleware/rabbitmq"
	rediscache "feedsystem_video_go/internal/middleware/redis"
	"regexp"
	"strings"
	"time"

	"gorm.io/gorm"
)

type CommentService struct {
	repo            *CommentRepository
	VideoRepository *VideoRepository
	cache           *rediscache.Client
	commentMQ       *rabbitmq.CommentMQ
	popularityMQ    *rabbitmq.PopularityMQ
}

func NewCommentService(repo *CommentRepository, videoRepo *VideoRepository, cache *rediscache.Client, commentMQ *rabbitmq.CommentMQ, popularityMQ *rabbitmq.PopularityMQ) *CommentService {
	return &CommentService{repo: repo, VideoRepository: videoRepo, cache: cache, commentMQ: commentMQ, popularityMQ: popularityMQ}
}

func (s *CommentService) Publish(ctx context.Context, comment *Comment) error {
	if comment == nil {
		return errors.New("comment is nil")
	}
	comment.Username = strings.TrimSpace(comment.Username)
	comment.Content = strings.TrimSpace(comment.Content)
	if comment.VideoID == 0 || comment.AuthorID == 0 {
		return errors.New("video_id and author_id are required")
	}
	if comment.Content == "" {
		return errors.New("content is required")
	}
	release, err := interactionWrites.acquire(ctx, comment.VideoID)
	if err != nil {
		return err
	}
	defer release()

	// 评论、热度事件、@mention 通知在同一个事务里落库，提交后由 outbox
	// dispatcher 负责投递。HTTP 返回时评论一定可见，且不会出现"评论成功但
	// 事件/通知丢失"的部分提交。
	err = s.repo.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Select("id").First(&Video{}, comment.VideoID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errors.New("video not found")
			}
			return err
		}
		if err := tx.Create(comment).Error; err != nil {
			return err
		}
		if err := enqueueComment(tx, comment); err != nil {
			return err
		}
		// @mention 通知和评论同事务：提交成功的评论不会丢 mention，
		// 插入失败则整体回滚，避免出现"评论已可见但通知永远没发"。
		if err := enqueueMentions(tx, comment); err != nil {
			return err
		}
		return tx.Model(&Video{}).Where("id = ?", comment.VideoID).
			UpdateColumn("popularity", gorm.Expr("popularity + 1")).Error
	})
	if err != nil {
		return err
	}
	return nil
}

func (s *CommentService) Delete(ctx context.Context, commentID uint, accountID uint) error {
	comment, err := s.repo.GetByID(ctx, commentID)
	if err != nil {
		return err
	}
	if comment == nil {
		return errors.New("comment not found")
	}
	if comment.AuthorID != accountID {
		return apierror.ErrUnauthorized
	}
	if s.commentMQ != nil {
		if err := s.commentMQ.Delete(ctx, commentID); err == nil {
			return nil
		}
	}
	return s.repo.DeleteComment(ctx, comment)
}

func (s *CommentService) GetAll(ctx context.Context, videoID uint) ([]Comment, error) {
	exists, err := s.VideoRepository.IsExist(ctx, videoID)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, errors.New("video not found")
	}
	return s.repo.GetAllComments(ctx, videoID)
}

var mentionRegex = regexp.MustCompile(`@(\w+)`)

// mentionNotification 用显式 EventID 写入 notifications 表，和 MQ 消费者共用
// idx_notification_event 唯一索引：同一条 mention 被重复处理时不会重复插入。
type mentionNotification struct {
	RecipientID uint
	SenderID    uint
	Type        string
	TargetID    uint
	Content     string
	EventID     *string
	CreatedAt   time.Time
}

func enqueueMentions(tx *gorm.DB, comment *Comment) error {
	matches := mentionRegex.FindAllStringSubmatch(comment.Content, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]bool)
	notifications := make([]mentionNotification, 0, len(matches))
	for _, m := range matches {
		username := m[1]
		if seen[username] || username == comment.Username {
			continue
		}
		seen[username] = true
		var accID uint
		if err := tx.Table("accounts").Where("username = ?", username).Select("id").Scan(&accID).Error; err != nil {
			return err
		}
		if accID == 0 {
			continue
		}
		eventID, err := events.NewID()
		if err != nil {
			return err
		}
		notifications = append(notifications, mentionNotification{
			RecipientID: accID,
			SenderID:    comment.AuthorID,
			Type:        "mention",
			TargetID:    comment.VideoID,
			Content:     comment.Username + " 在评论中提到了你",
			EventID:     &eventID,
			CreatedAt:   time.Now(),
		})
	}
	if len(notifications) == 0 {
		return nil
	}
	return tx.Table("notifications").Create(&notifications).Error
}
