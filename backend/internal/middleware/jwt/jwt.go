package jwt

import (
	"errors"
	"net/http"
	"strings"

	"feedsystem_video_go/internal/apierror"
	"feedsystem_video_go/internal/auth"

	"github.com/gin-gonic/gin"
)

func JWTAuth(validator auth.TokenValidator) gin.HandlerFunc {
	return requireToken(validator, false, false)
}

func SoftJWTAuth(validator auth.TokenValidator) gin.HandlerFunc {
	return requireToken(validator, true, false)
}

func QueryJWTAuth(validator auth.TokenValidator) gin.HandlerFunc {
	return requireToken(validator, false, true)
}

func requireToken(validator auth.TokenValidator, optional, allowQuery bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := ""
		if allowQuery {
			token = c.Query("token")
		}
		if token == "" {
			header := c.GetHeader("Authorization")
			if header == "" {
				if optional {
					c.Next()
					return
				}
				AbortAuthentication(c, apierror.ErrUnauthorized)
				return
			}
			parts := strings.Fields(header)
			if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
				AbortAuthentication(c, apierror.ErrUnauthorized)
				return
			}
			token = parts[1]
		}
		claims, err := validator.ValidateAccessToken(c.Request.Context(), token)
		if err != nil {
			AbortAuthentication(c, err)
			return
		}
		c.Set("accountID", claims.AccountID)
		c.Set("username", claims.Username)
		c.Set("sessionID", claims.SessionID)
		c.Set("accessToken", token)
		c.Set("accessClaims", claims)
		c.Next()
	}
}

func AbortAuthentication(c *gin.Context, err error) {
	if errors.Is(err, apierror.ErrServiceUnavailable) {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": apierror.ErrServiceUnavailable.Error()})
		return
	}
	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid, expired or revoked token"})
}

func GetAccountID(c *gin.Context) (uint, error) {
	value, exists := c.Get("accountID")
	accountID, ok := value.(uint)
	if !exists || !ok || accountID == 0 {
		return 0, apierror.ErrUnauthorized
	}
	return accountID, nil
}

func GetUsername(c *gin.Context) (string, error) {
	value, exists := c.Get("username")
	username, ok := value.(string)
	if !exists || !ok {
		return "", apierror.ErrUnauthorized
	}
	return username, nil
}
