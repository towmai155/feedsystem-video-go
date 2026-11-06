package video

import (
	"context"
	"errors"
	"feedsystem_video_go/internal/dbretry"
	"feedsystem_video_go/internal/middleware/rabbitmq"
	rediscache "feedsystem_video_go/internal/middleware/redis"
	"time"

	"github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type LikeService struct {
	repo         *LikeRepository
	VideoRepo    *VideoRepository
	cache        *rediscache.Client
	likeMQ       *rabbitmq.LikeMQ
	popularityMQ *rabbitmq.PopularityMQ
}

func NewLikeService(repo *LikeRepository, videoRepo *VideoRepository, cache *rediscache.Client, likeMQ *rabbitmq.LikeMQ, popularityMQ *rabbitmq.PopularityMQ) *LikeService {
	return &LikeService{repo: repo, VideoRepo: videoRepo, cache: cache, likeMQ: likeMQ, popularityMQ: popularityMQ}
}

// 幂等语义：重复点赞 / 重复取消不是错误，handler 据此返回 200 而不是 500。
var (
	ErrAlreadyLiked = errors.New("user has liked this video")
	ErrNotLiked     = errors.New("user has not liked this video")
)

func isDupKey(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}

func (s *LikeService) Like(ctx context.Context, like *Like) error {
	if like == nil {
		return errors.New("like is nil")
	}
	if like.VideoID == 0 || like.AccountID == 0 {
		return errors.New("video_id and account_id are required")
	}

	like.CreatedAt = time.Now()
	release, err := interactionWrites.acquire(ctx, like.VideoID)
	if err != nil {
		return err
	}
	defer release()

	// 先在一个事务里把"点赞行 + 计数"落库：唯一索引 (video_id, account_id) 负责去重，
	// ON DUPLICATE KEY UPDATE 让并发重复点赞只有一次生效，不再依赖先查后写的竞态。
	// 同一个视频上的并发事务偶尔会被 InnoDB 判为死锁（1213），整笔事务已经回滚，
	// 直接重跑即可；重跑前清掉上次试写留下的自增主键。
	err = dbretry.Run(ctx, func() error {
		like.ID = 0
		return s.repo.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			res := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(like)
			if res.Error != nil {
				if isDupKey(res.Error) {
					return ErrAlreadyLiked
				}
				return res.Error
			}
			if res.RowsAffected == 0 {
				return ErrAlreadyLiked
			}
			if err := enqueueLike(tx, like, "like", 1); err != nil {
				return err
			}
			// 不再单独 SELECT 一次视频：计数器更新自身就要求视频行存在，
			// 用它的影响行数判断 404，省掉一条语句和一次行锁。
			upd := tx.Model(&Video{}).Where("id = ?", like.VideoID).
				Updates(map[string]any{
					"likes_count": gorm.Expr("likes_count + 1"),
					"popularity":  gorm.Expr("popularity + 1"),
				})
			if upd.Error != nil {
				return upd.Error
			}
			if upd.RowsAffected == 0 {
				return errors.New("video not found")
			}
			return nil
		})
	})
	if err != nil {
		return err
	}

	// The transaction also persisted both events. Publication is retried by the
	// outbox dispatcher independently of this request or broker availability.
	return nil
}

func (s *LikeService) Unlike(ctx context.Context, like *Like) error {
	if like == nil {
		return errors.New("like is nil")
	}
	if like.VideoID == 0 || like.AccountID == 0 {
		return errors.New("video_id and account_id are required")
	}
	release, err := interactionWrites.acquire(ctx, like.VideoID)
	if err != nil {
		return err
	}
	defer release()

	// 同样先落库再发事件：删除行与减计数在同一事务内完成，
	// 并发的重复取消只会有一次真正删除成功。
	err = dbretry.Run(ctx, func() error {
		return s.repo.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			del := tx.Where("video_id = ? AND account_id = ?", like.VideoID, like.AccountID).Delete(&Like{})
			if del.Error != nil {
				return del.Error
			}
			if del.RowsAffected == 0 {
				return ErrNotLiked
			}
			if err := enqueueLike(tx, like, "unlike", -1); err != nil {
				return err
			}
			return tx.Model(&Video{}).Where("id = ?", like.VideoID).
				Updates(map[string]any{
					"likes_count": gorm.Expr("GREATEST(likes_count - 1, 0)"),
					"popularity":  gorm.Expr("GREATEST(popularity - 1, 0)"),
				}).Error
		})
	})
	if err != nil {
		return err
	}

	return nil
}

func (s *LikeService) IsLiked(ctx context.Context, videoID, accountID uint) (bool, error) {
	return s.repo.IsLiked(ctx, videoID, accountID)
}

func (s *LikeService) ListLikedVideos(ctx context.Context, accountID uint) ([]Video, error) {
	return s.repo.ListLikedVideos(ctx, accountID)
}
