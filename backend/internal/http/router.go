package http

import (
	"context"
	"feedsystem_video_go/internal/account"
	"feedsystem_video_go/internal/config"
	"feedsystem_video_go/internal/feed"
	"feedsystem_video_go/internal/message"
	"feedsystem_video_go/internal/middleware/jwt"
	"feedsystem_video_go/internal/middleware/rabbitmq"
	"feedsystem_video_go/internal/middleware/ratelimit"
	rediscache "feedsystem_video_go/internal/middleware/redis"
	"feedsystem_video_go/internal/social"
	"feedsystem_video_go/internal/video"
	"feedsystem_video_go/internal/worker"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"log"
	"time"
)

func SetRouter(db *gorm.DB, cache *rediscache.Client, rmq *rabbitmq.RabbitMQ, mqConfigs ...*config.RabbitMQConfig) *gin.Engine {
	r := gin.Default()
	if err := r.SetTrustedProxies(nil); err != nil {
		log.Printf("SetTrustedProxies failed: %v", err)
	}
	r.Static("/static", "./.run/uploads")
	// rate_limit
	loginLimiter := ratelimit.Limit(cache, "account_login", 10, time.Minute, ratelimit.KeyByIP)
	registerLimiter := ratelimit.Limit(cache, "account_register", 5, time.Hour, ratelimit.KeyByIP)

	likeLimiter := ratelimit.Limit(cache, "like_write", 30, time.Minute, ratelimit.KeyByAccount)
	commentLimiter := ratelimit.Limit(cache, "comment_write", 10, time.Minute, ratelimit.KeyByAccount)
	socialLimiter := ratelimit.Limit(cache, "social_write", 20, time.Minute, ratelimit.KeyByAccount)

	// account
	accountRepository := account.NewAccountRepository(db)
	sessionValidator := account.NewSessionValidator(accountRepository)
	sseHub := worker.NewSSEHub(db, sessionValidator)
	accountService := account.NewAccountService(accountRepository, sseHub.DisconnectSession)
	accountHandler := account.NewAccountHandler(accountService)
	accountGroup := r.Group("/account")
	{
		accountGroup.POST("/register", registerLimiter, accountHandler.CreateAccount)
		accountGroup.POST("/login", loginLimiter, accountHandler.Login)
		accountGroup.POST("/changePassword", accountHandler.ChangePassword)
		accountGroup.POST("/findByID", accountHandler.FindByID)
		accountGroup.POST("/findByUsername", accountHandler.FindByUsername)
		accountGroup.POST("/refresh", accountHandler.Refresh)
	}
	protectedAccountGroup := accountGroup.Group("")
	protectedAccountGroup.Use(jwt.JWTAuth(sessionValidator))
	{
		protectedAccountGroup.POST("/logout", accountHandler.Logout)
		protectedAccountGroup.POST("/rename", accountHandler.Rename)
		protectedAccountGroup.POST("/uploadAvatar", accountHandler.UploadAvatar)
		protectedAccountGroup.POST("/updateProfile", accountHandler.UpdateProfile)
	}
	// video
	videoRepository := video.NewVideoRepository(db)
	popularityMQ, err := rabbitmq.NewPopularityMQ(rmq)
	if err != nil {
		log.Printf("PopularityMQ init failed (mq disabled): %v", err)
		popularityMQ = nil
	}
	videoService := video.NewVideoService(videoRepository, cache, popularityMQ)
	videoHandler := video.NewVideoHandler(videoService, accountService)
	videoGroup := r.Group("/video")
	{
		videoGroup.POST("/listByAuthorID", videoHandler.ListByAuthorID)
		videoGroup.POST("/getDetail", videoHandler.GetDetail)
	}
	protectedVideoGroup := videoGroup.Group("")
	protectedVideoGroup.Use(jwt.JWTAuth(sessionValidator))
	{
		protectedVideoGroup.POST("/uploadVideo", videoHandler.UploadVideo)
		protectedVideoGroup.POST("/uploadCover", videoHandler.UploadCover)
		protectedVideoGroup.POST("/publish", videoHandler.PublishVideo)
		protectedVideoGroup.POST("/delete", videoHandler.DeleteVideo)
	}
	// like
	likeMQ, err := rabbitmq.NewLikeMQ(rmq)
	if err != nil {
		log.Printf("LikeMQ init failed (mq disabled): %v", err)
		likeMQ = nil
	}
	likeRepository := video.NewLikeRepository(db)
	likeService := video.NewLikeService(likeRepository, videoRepository, cache, likeMQ, popularityMQ)
	likeHandler := video.NewLikeHandler(likeService)
	likeGroup := r.Group("/like")
	protectedLikeGroup := likeGroup.Group("")
	protectedLikeGroup.Use(jwt.JWTAuth(sessionValidator))
	{
		protectedLikeGroup.POST("/like", likeLimiter, likeHandler.Like)
		protectedLikeGroup.POST("/unlike", likeLimiter, likeHandler.Unlike)
		protectedLikeGroup.POST("/isLiked", likeHandler.IsLiked)
		protectedLikeGroup.POST("/listMyLikedVideos", likeHandler.ListMyLikedVideos)
	}
	// comment
	commentRepository := video.NewCommentRepository(db)
	commentMQ, err := rabbitmq.NewCommentMQ(rmq)
	if err != nil {
		log.Printf("CommentMQ init failed (mq disabled): %v", err)
		commentMQ = nil
	}
	commentService := video.NewCommentService(commentRepository, videoRepository, cache, commentMQ, popularityMQ)
	commentHandler := video.NewCommentHandler(commentService, accountService)
	commentGroup := r.Group("/comment")
	{
		commentGroup.POST("/listAll", commentHandler.GetAllComments)
	}
	protectedCommentGroup := commentGroup.Group("")
	protectedCommentGroup.Use(jwt.JWTAuth(sessionValidator))
	{
		protectedCommentGroup.POST("/publish", commentLimiter, commentHandler.PublishComment)
		protectedCommentGroup.POST("/delete", commentLimiter, commentHandler.DeleteComment)
	}
	// social
	socialRepository := social.NewSocialRepository(db)
	socialService := social.NewSocialService(socialRepository, accountRepository)
	socialHandler := social.NewSocialHandler(socialService)
	socialGroup := r.Group("/social")
	protectedSocialGroup := socialGroup.Group("")
	protectedSocialGroup.Use(jwt.JWTAuth(sessionValidator))
	{
		protectedSocialGroup.POST("/follow", socialLimiter, socialHandler.Follow)
		protectedSocialGroup.POST("/unfollow", socialLimiter, socialHandler.Unfollow)
		protectedSocialGroup.POST("/getAllFollowers", socialHandler.GetAllFollowers)
		protectedSocialGroup.POST("/getAllVloggers", socialHandler.GetAllVloggers)
		protectedSocialGroup.POST("/getCounts", socialHandler.GetCounts)
	}

	accountGroup.POST("/getProfile", func(c *gin.Context) {
		var req account.GetProfileRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		if req.AccountID == 0 {
			c.JSON(400, gin.H{"error": "account_id is required"})
			return
		}
		acc, err := accountService.FindByID(c.Request.Context(), req.AccountID)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		videoCount, _ := videoRepository.CountByAuthor(c.Request.Context(), req.AccountID)
		totalLikes, _ := videoRepository.TotalLikesByAuthor(c.Request.Context(), req.AccountID)
		followerCount, _ := socialRepository.CountFollowers(c.Request.Context(), req.AccountID)
		vloggerCount, _ := socialRepository.CountVloggers(c.Request.Context(), req.AccountID)

		c.JSON(200, account.GetProfileResponse{
			Account:    account.FindByIDResponse{ID: acc.ID, Username: acc.Username, AvatarURL: acc.AvatarURL, Bio: acc.Bio},
			VideoCount: videoCount, TotalLikes: totalLikes,
			FollowerCount: followerCount, VloggerCount: vloggerCount,
		})
	})
	// feed
	feedRepository := feed.NewFeedRepository(db)
	feedService := feed.NewFeedService(feedRepository, likeRepository, cache)
	feedHandler := feed.NewFeedHandler(feedService)
	feedGroup := r.Group("/feed")
	feedGroup.Use(jwt.SoftJWTAuth(sessionValidator))
	{
		feedGroup.POST("/listLatest", feedHandler.ListLatest)
		feedGroup.POST("/listLikesCount", feedHandler.ListLikesCount)
		feedGroup.POST("/listByPopularity", feedHandler.ListByPopularity)
		feedGroup.POST("/listByTag", feedHandler.ListByTag)
	}
	protectedFeedGroup := feedGroup.Group("")
	protectedFeedGroup.Use(jwt.JWTAuth(sessionValidator))
	{
		protectedFeedGroup.POST("/listByFollowing", feedHandler.ListByFollowing)
	}
	// message
	messageRepo := message.NewRepository(db)
	messageService := message.NewService(messageRepo)
	messageHandler := message.NewHandler(messageService)
	messageGroup := r.Group("/message")
	protectedMessageGroup := messageGroup.Group("")
	protectedMessageGroup.Use(jwt.JWTAuth(sessionValidator))
	{
		protectedMessageGroup.POST("/send", messageHandler.Send)
		protectedMessageGroup.POST("/list", messageHandler.List)
	}
	//worker
	timelineMQ, err := rabbitmq.NewTimelineMQ(rmq)
	if err != nil {
		log.Printf("timelineMQ init failed (mq disabled): %v", err)
		timelineMQ = nil
	}
	worker.StartOutboxPoller(db, timelineMQ)
	worker.StartConsumer(timelineMQ, "video.timeline.update.queue", cache)

	// SSE notification
	if rmq != nil && rmq.Ch != nil {
		rmq.DeclareTopic("like.events", "notification.like", "like.like")
		rmq.DeclareTopic("comment.events", "notification.comment", "comment.publish")
		rmq.DeclareTopic("social.events", "notification.social", "social.follow")
	}
	notifGroup := r.Group("/notification")
	notifGroup.Use(sseHub.SSERequireAuth())
	sseHub.RegisterRoutes(r, notifGroup)

	if len(mqConfigs) > 0 && mqConfigs[0] != nil {
		go worker.RunNotifications(context.Background(), *mqConfigs[0], db, sseHub)
	} else {
		go func() {
			if rmq != nil && rmq.Ch != nil {
				hub := sseHub
				ctx := context.Background()
				// Each consumer owns its own amqp channel: a single channel shared by
				// several goroutines is not safe in amqp091 and stalls under load.
				startNotificationConsumer := func(queue string) {
					ch, err := rmq.NewChannel()
					if err != nil {
						log.Printf("notification channel for %s failed: %v", queue, err)
						return
					}
					if err := ch.Qos(50, 0, false); err != nil {
						log.Printf("notification qos for %s failed: %v", queue, err)
					}
					w := worker.NewNotificationWorker(ch, db, queue, hub)
					if err := w.Run(ctx); err != nil {
						log.Printf("notification worker %s: %v", queue, err)
					}
				}
				go startNotificationConsumer("notification.like")
				go startNotificationConsumer("notification.comment")
				go startNotificationConsumer("notification.social")
			} else {
				log.Printf("Notification SSE disabled (MQ not available)")
			}
		}()
	}

	return r
}
