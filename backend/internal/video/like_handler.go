package video

import (
	"errors"

	"feedsystem_video_go/internal/apierror"
	"feedsystem_video_go/internal/middleware/jwt"

	"github.com/gin-gonic/gin"
)

type LikeHandler struct {
	service *LikeService
}

func NewLikeHandler(service *LikeService) *LikeHandler {
	return &LikeHandler{service: service}
}

func (lh *LikeHandler) Like(c *gin.Context) {
	var req LikeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	if req.VideoID <= 0 {
		c.JSON(400, gin.H{"error": "video_id is required"})
		return
	}

	accountID, err := jwt.GetAccountID(c)
	if err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}

	like := &Like{
		VideoID:   req.VideoID,
		AccountID: accountID,
	}
	if err := lh.service.Like(c.Request.Context(), like); err != nil {
		// 幂等：并发或重复点赞返回 200，只有真正的失败才是 5xx。
		if errors.Is(err, ErrAlreadyLiked) {
			c.JSON(200, gin.H{"message": "like success", "already_liked": true})
			return
		}
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"message": "like success"})
}

func (lh *LikeHandler) Unlike(c *gin.Context) {
	var req LikeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	if req.VideoID <= 0 {
		c.JSON(400, gin.H{"error": "video_id is required"})
		return
	}

	accountID, err := jwt.GetAccountID(c)
	if err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}

	like := &Like{
		VideoID:   req.VideoID,
		AccountID: accountID,
	}
	if err := lh.service.Unlike(c.Request.Context(), like); err != nil {
		// 幂等：没有点过赞时取消也返回 200。
		if errors.Is(err, ErrNotLiked) {
			c.JSON(200, gin.H{"message": "unlike success", "already_unliked": true})
			return
		}
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"message": "unlike success"})
}

func (lh *LikeHandler) IsLiked(c *gin.Context) {
	var req LikeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	if req.VideoID <= 0 {
		c.JSON(400, gin.H{"error": "video_id is required"})
		return
	}

	accountID, err := jwt.GetAccountID(c)
	if err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	isLiked, err := lh.service.IsLiked(c.Request.Context(), req.VideoID, accountID)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"is_liked": isLiked})
}

func (lh *LikeHandler) ListMyLikedVideos(c *gin.Context) {
	accountID, err := jwt.GetAccountID(c)
	if err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}

	videos, err := lh.service.ListLikedVideos(c.Request.Context(), accountID)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	if videos == nil {
		videos = []Video{}
	}
	c.JSON(200, videos)
}
