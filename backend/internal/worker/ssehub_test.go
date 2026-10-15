package worker

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"feedsystem_video_go/internal/account"
	"feedsystem_video_go/internal/apierror"
	"feedsystem_video_go/internal/auth"
	"feedsystem_video_go/internal/testutil"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

type streamValidatorFunc func(context.Context, string) (*auth.Claims, error)

func (f streamValidatorFunc) ValidateAccessToken(ctx context.Context, token string) (*auth.Claims, error) {
	return f(ctx, token)
}

func streamServer(t *testing.T, hub *SSEHub) *httptest.Server {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/stream", hub.SSERequireAuth(), hub.SSEHandler)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return server
}

func openStream(t *testing.T, server *httptest.Server, token string) (io.ReadCloser, *bufio.Reader) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/stream?token="+url.QueryEscape(token), nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { response.Body.Close() })
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		t.Fatalf("stream status: %d", response.StatusCode)
	}
	reader := bufio.NewReader(response.Body)
	for _, expected := range []string{": connected\n", "\n"} {
		line, err := reader.ReadString('\n')
		if err != nil || line != expected {
			t.Fatalf("missing stream readiness event: %q %v", line, err)
		}
	}
	return response.Body, reader
}

func requireStreamClosed(t *testing.T, reader io.Reader) {
	t.Helper()
	done := make(chan error, 1)
	go func() { _, err := io.ReadAll(reader); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("stream read failed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("revoked stream remained open")
	}
}

func streamFixture(t *testing.T) (*SSEHub, *account.AccountService, *httptest.Server) {
	t.Helper()
	t.Setenv("JWT_SECRET", "test-signing-key")
	db := testutil.NewMySQL(t)
	if err := db.AutoMigrate(&account.Account{}); err != nil {
		t.Fatal(err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte("password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	repo := account.NewAccountRepository(db)
	if err := repo.CreateAccount(context.Background(), &account.Account{Username: "tester", Password: string(hash)}); err != nil {
		t.Fatal(err)
	}
	hub := NewSSEHub(db, account.NewSessionValidator(repo))
	hub.recheckInterval = 10 * time.Millisecond
	service := account.NewAccountService(repo, hub.DisconnectSession)
	return hub, service, streamServer(t, hub)
}

func streamLogin(t *testing.T, service *account.AccountService) (account.LoginResponse, *auth.Claims) {
	t.Helper()
	login, err := service.Login(context.Background(), "tester", "password")
	if err != nil {
		t.Fatal(err)
	}
	claims, err := auth.ParseToken(login.Token)
	if err != nil {
		t.Fatal(err)
	}
	return login, claims
}

func TestSSERegistrationRechecksRevokedSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	checks := 0
	validator := streamValidatorFunc(func(context.Context, string) (*auth.Claims, error) {
		checks++
		if checks > 1 {
			return nil, apierror.ErrUnauthorized
		}
		return &auth.Claims{AccountID: 7, SessionID: "session-one"}, nil
	})
	hub := NewSSEHub(nil, validator)
	router := gin.New()
	router.GET("/stream", hub.SSERequireAuth(), hub.SSEHandler)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/stream?token=token", nil))
	if response.Code != http.StatusUnauthorized || checks != 2 {
		t.Fatalf("registration race accepted: status=%d checks=%d", response.Code, checks)
	}
	if len(hub.clients) != 0 {
		t.Fatal("rejected stream subscription leaked")
	}
}

func TestSSELogoutClosesExistingConnection(t *testing.T) {
	hub, service, server := streamFixture(t)
	login, claims := streamLogin(t, service)
	_, reader := openStream(t, server, login.Token)
	if err := service.Logout(context.Background(), login.AccountID, claims.SessionID); err != nil {
		t.Fatal(err)
	}
	hub.Push(login.AccountID, &Notification{Content: "must not be delivered"})
	requireStreamClosed(t, reader)
}

func TestSSENewLoginClosesOldConnectionAndOldLogoutPreservesNewOne(t *testing.T) {
	hub, service, server := streamFixture(t)
	old, oldClaims := streamLogin(t, service)
	_, oldReader := openStream(t, server, old.Token)
	current, _ := streamLogin(t, service)
	requireStreamClosed(t, oldReader)
	_, currentReader := openStream(t, server, current.Token)
	if err := service.Logout(context.Background(), old.AccountID, oldClaims.SessionID); err != nil {
		t.Fatal(err)
	}
	hub.Push(current.AccountID, &Notification{Content: "new session still active"})
	line, err := currentReader.ReadString('\n')
	if err != nil || !strings.Contains(line, "new session still active") {
		t.Fatalf("new stream incorrectly closed: %q %v", line, err)
	}
}

func TestSSERemoteRevocationIsDetected(t *testing.T) {
	hub, service, server := streamFixture(t)
	login, claims := streamLogin(t, service)
	_, reader := openStream(t, server, login.Token)
	remote := account.NewAccountService(account.NewAccountRepository(hub.db), nil)
	if err := remote.Logout(context.Background(), login.AccountID, claims.SessionID); err != nil {
		t.Fatal(err)
	}
	requireStreamClosed(t, reader)
}

func TestSSEClosesAtAccessTokenExpiry(t *testing.T) {
	expires := time.Now().Add(150 * time.Millisecond)
	validator := streamValidatorFunc(func(context.Context, string) (*auth.Claims, error) {
		return &auth.Claims{AccountID: 7, SessionID: "session-one", RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(expires)}}, nil
	})
	hub := NewSSEHub(nil, validator)
	server := streamServer(t, hub)
	_, reader := openStream(t, server, "token")
	requireStreamClosed(t, reader)
}

func TestSSEPublishDisconnectAndUnsubscribeAreSafeConcurrently(t *testing.T) {
	hub := NewSSEHub(nil, nil)
	newSession := hub.subscribe(context.Background(), 7, "session-two")
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			hub.Push(7, &Notification{})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			client := hub.subscribe(context.Background(), 7, "session-one")
			hub.DisconnectSession(7, "session-one")
			hub.unsubscribe(client)
		}
	}()
	wg.Wait()
	if newSession.ctx.Err() != nil {
		t.Fatal("disconnecting old sessions cancelled a new session")
	}
	hub.unsubscribe(newSession)
	if len(hub.clients) != 0 {
		t.Fatal("subscription leaked")
	}
}
