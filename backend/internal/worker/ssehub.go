package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"feedsystem_video_go/internal/apierror"
	"feedsystem_video_go/internal/auth"
	jwtmiddleware "feedsystem_video_go/internal/middleware/jwt"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type SSEHub struct {
	mu              sync.RWMutex
	clients         map[uint]map[*sseClient]struct{}
	db              *gorm.DB
	validator       auth.TokenValidator
	recheckInterval time.Duration
}

type sseClient struct {
	userID        uint
	sessionID     string
	notifications chan *Notification
	ctx           context.Context
	cancel        context.CancelFunc
}

func NewSSEHub(db *gorm.DB, validator auth.TokenValidator) *SSEHub {
	return &SSEHub{
		clients: make(map[uint]map[*sseClient]struct{}), db: db,
		validator: validator, recheckInterval: time.Second,
	}
}

func (h *SSEHub) Push(userID uint, n *Notification) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for client := range h.clients[userID] {
		select {
		case <-client.ctx.Done():
		case client.notifications <- n:
		default:
		}
	}
}

func (h *SSEHub) subscribe(ctx context.Context, userID uint, sessionID string) *sseClient {
	clientCtx, cancel := context.WithCancel(ctx)
	client := &sseClient{userID: userID, sessionID: sessionID,
		notifications: make(chan *Notification, 20), ctx: clientCtx, cancel: cancel}
	h.mu.Lock()
	if h.clients[userID] == nil {
		h.clients[userID] = make(map[*sseClient]struct{})
	}
	h.clients[userID][client] = struct{}{}
	h.mu.Unlock()
	return client
}

func (h *SSEHub) unsubscribe(client *sseClient) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.clients[client.userID], client)
	if len(h.clients[client.userID]) == 0 {
		delete(h.clients, client.userID)
	}
	// Notification channels are never closed: concurrent publishers cannot send
	// on a closed channel. Cancellation wakes the stream handler instead.
	client.cancel()
}

func (h *SSEHub) DisconnectSession(userID uint, sessionID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for client := range h.clients[userID] {
		if client.sessionID == sessionID {
			client.cancel()
			delete(h.clients[userID], client)
		}
	}
	if len(h.clients[userID]) == 0 {
		delete(h.clients, userID)
	}
}

func (h *SSEHub) SSERequireAuth() gin.HandlerFunc {
	return jwtmiddleware.QueryJWTAuth(h.validator)
}

func (h *SSEHub) SSEHandler(c *gin.Context) {
	userID, err := jwtmiddleware.GetAccountID(c)
	if err != nil {
		jwtmiddleware.AbortAuthentication(c, err)
		return
	}
	token := c.GetString("accessToken")
	client := h.subscribe(c.Request.Context(), userID, c.GetString("sessionID"))
	defer h.unsubscribe(client)
	// Register before rechecking, covering a logout between middleware validation
	// and registration. A local revocation cancels this query as well.
	claims, err := h.validator.ValidateAccessToken(client.ctx, token)
	if err != nil {
		if client.ctx.Err() != nil && c.Request.Context().Err() == nil {
			err = apierror.ErrUnauthorized
		}
		jwtmiddleware.AbortAuthentication(c, err)
		return
	}
	if claims.ExpiresAt == nil {
		jwtmiddleware.AbortAuthentication(c, apierror.ErrUnauthorized)
		return
	}
	if client.ctx.Err() != nil {
		return
	}

	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.WriteHeader(http.StatusOK)

	flusher, _ := c.Writer.(http.Flusher)
	writeEvent := func(text string) bool {
		if _, err := fmt.Fprint(c.Writer, text); err != nil {
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}
	if !writeEvent(": connected\n\n") {
		return
	}
	recheck := time.NewTicker(h.recheckInterval)
	defer recheck.Stop()
	heartbeat := time.NewTicker(30 * time.Second)
	defer heartbeat.Stop()
	expiry := time.NewTimer(time.Until(claims.ExpiresAt.Time))
	defer expiry.Stop()

	for {
		select {
		case <-client.ctx.Done():
			return
		case <-expiry.C:
			return
		case n := <-client.notifications:
			if _, err := h.validator.ValidateAccessToken(client.ctx, token); err != nil || client.ctx.Err() != nil {
				return
			}
			b, err := json.Marshal(n)
			if err != nil {
				continue
			}
			if !writeEvent("data: " + string(b) + "\n\n") {
				return
			}
		case <-recheck.C:
			// Also detect revocations committed by another API instance.
			if _, err := h.validator.ValidateAccessToken(client.ctx, token); err != nil {
				return
			}
		case <-heartbeat.C:
			if !writeEvent(": keepalive\n\n") {
				return
			}
		}
	}
}

func (h *SSEHub) ListHandler(c *gin.Context) {
	accountID, _ := c.Get("accountID")
	userID := accountID.(uint)

	var notifications []Notification
	if err := h.db.WithContext(c.Request.Context()).
		Where("recipient_id = ?", userID).
		Order("created_at desc").
		Limit(50).
		Find(&notifications).Error; err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	if notifications == nil {
		notifications = []Notification{}
	}
	c.JSON(200, gin.H{"notifications": notifications})
}

func (h *SSEHub) MarkReadHandler(c *gin.Context) {
	accountID, _ := c.Get("accountID")
	userID := accountID.(uint)

	var req struct {
		ID *uint `json:"id"`
	}
	c.ShouldBindJSON(&req)

	if req.ID != nil {
		h.db.WithContext(c.Request.Context()).Model(&Notification{}).Where("id = ? AND recipient_id = ?", *req.ID, userID).Update("is_read", true)
	} else {
		h.db.WithContext(c.Request.Context()).Model(&Notification{}).Where("recipient_id = ?", userID).Update("is_read", true)
	}
	c.JSON(200, gin.H{"message": "ok"})
}

func (h *SSEHub) UnreadCountHandler(c *gin.Context) {
	accountID, _ := c.Get("accountID")
	userID := accountID.(uint)

	var count int64
	h.db.WithContext(c.Request.Context()).Model(&Notification{}).Where("recipient_id = ? AND is_read = ?", userID, false).Count(&count)
	c.JSON(200, gin.H{"count": count})
}

func (h *SSEHub) RegisterRoutes(r *gin.Engine, group *gin.RouterGroup) {
	group.GET("/stream", h.SSEHandler)
	group.POST("/list", h.ListHandler)
	group.POST("/markRead", h.MarkReadHandler)
	group.POST("/unreadCount", h.UnreadCountHandler)
}

var _ NotificationHub = (*SSEHub)(nil)
