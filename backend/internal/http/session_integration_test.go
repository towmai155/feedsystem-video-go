package http_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	stdhttp "net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"feedsystem_video_go/internal/account"
	"feedsystem_video_go/internal/config"
	apphttp "feedsystem_video_go/internal/http"
	rediscache "feedsystem_video_go/internal/middleware/redis"
	"feedsystem_video_go/internal/testutil"
	"feedsystem_video_go/internal/worker"

	miniredis "github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

func sessionRouter(t *testing.T, cache *rediscache.Client) (*gin.Engine, *gorm.DB) {
	t.Helper()
	t.Setenv("JWT_SECRET", "test-signing-key")
	gin.SetMode(gin.TestMode)
	db := testutil.NewMySQL(t)
	if err := db.AutoMigrate(&account.Account{}, &worker.Notification{}); err != nil {
		t.Fatal(err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte("password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if err := account.NewAccountRepository(db).CreateAccount(context.Background(), &account.Account{Username: "tester", Password: string(hash)}); err != nil {
		t.Fatal(err)
	}
	return apphttp.SetRouter(db, nil, cache, nil), db
}

func postAccount(t *testing.T, router *gin.Engine, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(stdhttp.MethodPost, path, bytes.NewReader(encoded))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	return response
}

func accountLogin(t *testing.T, router *gin.Engine) account.LoginResponse {
	t.Helper()
	response := postAccount(t, router, "/account/login", "", map[string]string{"username": "tester", "password": "password"})
	if response.Code != stdhttp.StatusOK {
		t.Fatalf("login status=%d body=%s", response.Code, response.Body.String())
	}
	var login account.LoginResponse
	if err := json.Unmarshal(response.Body.Bytes(), &login); err != nil {
		t.Fatal(err)
	}
	return login
}

func TestLogoutIgnoresStaleOrUnavailableRedisAndProtectsNotifications(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		t.Run(map[bool]string{false: "stale-redis", true: "redis-unavailable"}[unavailable], func(t *testing.T) {
			mr, err := miniredis.Run()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(mr.Close)
			host, portString, err := net.SplitHostPort(mr.Addr())
			if err != nil {
				t.Fatal(err)
			}
			port, err := strconv.Atoi(portString)
			if err != nil {
				t.Fatal(err)
			}
			cache, err := rediscache.NewFromEnv(&config.RedisConfig{Host: host, Port: port})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { cache.Close() })
			router, _ := sessionRouter(t, cache)
			login := accountLogin(t, router)
			key := cache.Key("account:%d", login.AccountID)
			mr.Set(key, login.Token)
			if unavailable {
				mr.Close()
			}
			response := postAccount(t, router, "/account/logout", login.Token, map[string]any{})
			if response.Code != stdhttp.StatusOK {
				t.Fatalf("logout status=%d", response.Code)
			}
			if !unavailable {
				if value, err := mr.Get(key); err != nil || value != login.Token {
					t.Fatal("test needs a stale Redis token")
				}
			}
			for _, path := range []string{"/account/updateProfile", "/notification/list", "/notification/unreadCount", "/notification/markRead"} {
				response := postAccount(t, router, path, login.Token, map[string]string{"bio": "should-not-change"})
				if response.Code != stdhttp.StatusUnauthorized {
					t.Fatalf("revoked token accepted by %s: %d", path, response.Code)
				}
			}
			stream := httptest.NewRecorder()
			router.ServeHTTP(stream, httptest.NewRequest(stdhttp.MethodGet, "/notification/stream?token="+url.QueryEscape(login.Token), nil))
			if stream.Code != stdhttp.StatusUnauthorized {
				t.Fatalf("revoked token opened stream: %d", stream.Code)
			}
		})
	}
}

func TestDatabaseFailureReturns503ForSessionEndpoints(t *testing.T) {
	router, db := sessionRouter(t, nil)
	login := accountLogin(t, router)
	connection, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path string
		body any
	}{
		{"/account/logout", map[string]any{}},
		{"/account/refresh", map[string]string{"refresh_token": login.RefreshToken}},
		{"/account/login", map[string]string{"username": "tester", "password": "password"}},
		{"/notification/list", map[string]any{}},
	} {
		response := postAccount(t, router, tc.path, login.Token, tc.body)
		if response.Code != stdhttp.StatusServiceUnavailable {
			t.Fatalf("%s status=%d body=%s", tc.path, response.Code, response.Body.String())
		}
		if bytes.Contains(response.Body.Bytes(), []byte("database is closed")) {
			t.Fatal("database internals leaked")
		}
	}
}
