package social

import (
	"context"
	"errors"
	"time"

	"feedsystem_video_go/internal/account"
	"feedsystem_video_go/internal/dbretry"
	"feedsystem_video_go/internal/events"
	"feedsystem_video_go/internal/middleware/rabbitmq"

	"github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
)

type SocialService struct {
	repo        *SocialRepository
	accountrepo *account.AccountRepository
}

func NewSocialService(repo *SocialRepository, accountrepo *account.AccountRepository) *SocialService {
	return &SocialService{repo: repo, accountrepo: accountrepo}
}

// Follow 把关注关系和它的通知事件写在同一个事务里。
// 之前是"先发 MQ（且忽略错误）再写库"：broker 故障时关注成功但通知永久丢失，
// 写库失败时又会留下没有关注关系的通知；每次请求生成新 EventID，重试还会重复通知。
// 现在事件走事务性 outbox，和点赞/评论一致。
func (s *SocialService) Follow(ctx context.Context, social *Social) error {
	_, err := s.accountrepo.FindByID(ctx, social.FollowerID)
	if err != nil {
		return err
	}
	_, err = s.accountrepo.FindByID(ctx, social.VloggerID)
	if err != nil {
		return err
	}
	if social.FollowerID == social.VloggerID {
		return errors.New("can not follow self")
	}
	isFollowed, err := s.repo.IsFollowed(ctx, social)
	if err != nil {
		return err
	}
	if isFollowed {
		return errors.New("already followed")
	}
	return dbretry.Run(ctx, func() error {
		social.ID = 0
		return s.repo.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if err := tx.Create(social).Error; err != nil {
				// 并发重复关注：唯一索引兜底，按幂等语义返回。
				if isDuplicateKey(err) {
					return errors.New("already followed")
				}
				return err
			}
			return enqueueSocialEvent(tx, "follow", social)
		})
	})
}

func (s *SocialService) Unfollow(ctx context.Context, social *Social) error {
	_, err := s.accountrepo.FindByID(ctx, social.FollowerID)
	if err != nil {
		return err
	}
	_, err = s.accountrepo.FindByID(ctx, social.VloggerID)
	if err != nil {
		return err
	}
	isFollowed, err := s.repo.IsFollowed(ctx, social)
	if err != nil {
		return err
	}
	if !isFollowed {
		return errors.New("not followed")
	}
	return dbretry.Run(ctx, func() error {
		return s.repo.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			res := tx.Where("follower_id = ? AND vlogger_id = ?", social.FollowerID, social.VloggerID).
				Delete(&Social{})
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected == 0 {
				// 并发取消关注，另一请求已经把关系删掉了。
				return errors.New("not followed")
			}
			return enqueueSocialEvent(tx, "unfollow", social)
		})
	})
}

func enqueueSocialEvent(tx *gorm.DB, action string, social *Social) error {
	id, err := events.NewID()
	if err != nil {
		return err
	}
	return events.Enqueue(tx, id, "social.events", "social."+action, rabbitmq.SocialEvent{
		EventID:    id,
		Action:     action,
		FollowerID: social.FollowerID,
		VloggerID:  social.VloggerID,
		OccurredAt: time.Now().UTC(),
	})
}

func isDuplicateKey(err error) bool {
	var mysqlErr *mysql.MySQLError
	return errors.As(err, &mysqlErr) && mysqlErr.Number == 1062
}

func (s *SocialService) GetAllFollowers(ctx context.Context, VloggerID uint) ([]*account.Account, error) {
	_, err := s.accountrepo.FindByID(ctx, VloggerID)
	if err != nil {
		return nil, err
	}
	return s.repo.GetAllFollowers(ctx, VloggerID)
}

func (s *SocialService) GetAllVloggers(ctx context.Context, FollowerID uint) ([]*account.Account, error) {
	_, err := s.accountrepo.FindByID(ctx, FollowerID)
	if err != nil {
		return nil, err
	}
	return s.repo.GetAllVloggers(ctx, FollowerID)
}

func (s *SocialService) CountFollowers(ctx context.Context, vloggerID uint) (int64, error) {
	return s.repo.CountFollowers(ctx, vloggerID)
}

func (s *SocialService) CountVloggers(ctx context.Context, followerID uint) (int64, error) {
	return s.repo.CountVloggers(ctx, followerID)
}

func (s *SocialService) IsFollowed(ctx context.Context, social *Social) (bool, error) {
	_, err := s.accountrepo.FindByID(ctx, social.FollowerID)
	if err != nil {
		return false, err
	}
	_, err = s.accountrepo.FindByID(ctx, social.VloggerID)
	if err != nil {
		return false, err
	}
	return s.repo.IsFollowed(ctx, social)
}
