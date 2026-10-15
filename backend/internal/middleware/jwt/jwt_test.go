package jwt

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"feedsystem_video_go/internal/apierror"
	"feedsystem_video_go/internal/auth"

	"github.com/gin-gonic/gin"
)

type validatorFunc func(context.Context, string) (*auth.Claims, error)

func (f validatorFunc) ValidateAccessToken(ctx context.Context, token string) (*auth.Claims, error) {
	return f(ctx, token)
}

func TestAuthenticationModesAndStoreFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name, header, query string
		soft                bool
		validateErr         error
		status, reads       int
	}{
		{name: "missing-token", status: 401},
		{name: "guest", soft: true, status: 204},
		{name: "malformed-header", header: "Basic token", status: 401},
		{name: "valid", header: "Bearer token", status: 204, reads: 1},
		{name: "query-token", query: "?token=token", status: 204, reads: 1},
		{name: "soft-invalid", soft: true, header: "Bearer token", validateErr: apierror.ErrUnauthorized, status: 401, reads: 1},
		{name: "store-failure", header: "Bearer token", validateErr: fmt.Errorf("%w: database-private-detail", apierror.ErrServiceUnavailable), status: 503, reads: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads := 0
			validator := validatorFunc(func(context.Context, string) (*auth.Claims, error) {
				reads++
				return &auth.Claims{AccountID: 7, Username: "tester", SessionID: "session-one"}, tc.validateErr
			})
			router := gin.New()
			middleware := JWTAuth(validator)
			if tc.soft {
				middleware = SoftJWTAuth(validator)
			}
			if tc.query != "" {
				middleware = QueryJWTAuth(validator)
			}
			router.GET("/", middleware, func(c *gin.Context) {
				if tc.reads > 0 && (c.GetString("sessionID") != "session-one" || c.GetString("username") != "tester") {
					t.Error("missing authenticated context")
				}
				c.Status(http.StatusNoContent)
			})
			req := httptest.NewRequest(http.MethodGet, "/"+tc.query, nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, req)
			if response.Code != tc.status || reads != tc.reads {
				t.Fatalf("status=%d reads=%d; want %d,%d", response.Code, reads, tc.status, tc.reads)
			}
			if strings.Contains(response.Body.String(), "database-private-detail") {
				t.Fatal("database error leaked to caller")
			}
		})
	}
}
