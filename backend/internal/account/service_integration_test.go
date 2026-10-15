package account

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"feedsystem_video_go/internal/apierror"
	"feedsystem_video_go/internal/auth"
	"feedsystem_video_go/internal/testutil"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

func sessionFixture(t *testing.T) (*AccountRepository, *AccountService, *SessionValidator) {
	t.Helper()
	t.Setenv("JWT_SECRET", "test-signing-key")
	db := testutil.NewMySQL(t)
	if err := db.AutoMigrate(&Account{}); err != nil {
		t.Fatal(err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte("old-password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	repo := NewAccountRepository(db)
	if err := repo.CreateAccount(context.Background(), &Account{Username: "tester", Password: string(hash)}); err != nil {
		t.Fatal(err)
	}
	return repo, NewAccountService(repo, nil), NewSessionValidator(repo)
}

func loginForTest(t *testing.T, service *AccountService) (LoginResponse, *auth.Claims) {
	t.Helper()
	login, err := service.Login(context.Background(), "tester", "old-password")
	if err != nil {
		t.Fatal(err)
	}
	claims, err := auth.ParseToken(login.Token)
	if err != nil {
		t.Fatal(err)
	}
	return login, claims
}

func waitResult[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case result := <-ch:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for synchronized operation")
		var zero T
		return zero
	}
}

// Pause after the actual MySQL read has returned, without using timing sleeps.
func pauseRefreshRead(t *testing.T, db *gorm.DB) (context.Context, <-chan struct{}, func()) {
	t.Helper()
	type pauseKey struct{}
	ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), pauseKey{}, true), 5*time.Second)
	started, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := sync.OnceFunc(func() { close(resume) })
	const callback = "test:pause_refresh_read"
	if err := db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Context.Value(pauseKey{}) == true {
			once.Do(func() {
				close(started)
				select {
				case <-resume:
				case <-ctx.Done():
				}
			})
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { release(); cancel(); db.Callback().Query().Remove(callback) })
	return ctx, started, release
}

func TestSessionLifecycle(t *testing.T) {
	repo, service, validator := sessionFixture(t)
	login, claims := loginForTest(t, service)
	ctx := context.Background()
	if _, err := validator.ValidateAccessToken(ctx, login.Token); err != nil {
		t.Fatal(err)
	}
	token, _, _, err := service.RefreshAccessToken(ctx, login.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	refreshed, err := auth.ParseToken(token)
	if err != nil || refreshed.SessionID != claims.SessionID {
		t.Fatalf("refresh changed session: %v", err)
	}
	if _, err := validator.ValidateAccessToken(ctx, login.Token); !errors.Is(err, apierror.ErrUnauthorized) {
		t.Fatal("superseded access token accepted")
	}
	if _, err := validator.ValidateAccessToken(ctx, token); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := service.Logout(ctx, login.AccountID, claims.SessionID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := validator.ValidateAccessToken(ctx, token); !errors.Is(err, apierror.ErrUnauthorized) {
		t.Fatal("logged-out access token accepted")
	}
	if _, _, _, err := service.RefreshAccessToken(ctx, login.RefreshToken); !errors.Is(err, apierror.ErrUnauthorized) {
		t.Fatal("logged-out refresh token accepted")
	}
	current, err := repo.FindByID(ctx, login.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Token != "" || current.SessionID != "" || current.RefreshToken != "" || current.RefreshExpiresAt != nil {
		t.Fatal("session not fully revoked")
	}
}

func TestPendingRefreshCannotRestoreLoggedOutSession(t *testing.T) {
	repo, service, validator := sessionFixture(t)
	login, claims := loginForTest(t, service)
	ctx, started, release := pauseRefreshRead(t, repo.db)
	result := make(chan error, 1)
	go func() { _, _, _, err := service.RefreshAccessToken(ctx, login.RefreshToken); result <- err }()
	waitResult(t, started)
	if err := service.Logout(context.Background(), login.AccountID, claims.SessionID); err != nil {
		t.Fatal(err)
	}
	release()
	if err := waitResult(t, result); !errors.Is(err, apierror.ErrUnauthorized) {
		t.Fatalf("pending refresh succeeded after logout: %v", err)
	}
	if _, err := validator.ValidateAccessToken(context.Background(), login.Token); !errors.Is(err, apierror.ErrUnauthorized) {
		t.Fatal("session restored after logout")
	}
}

func TestPendingRefreshCannotOverwriteNewLogin(t *testing.T) {
	repo, service, validator := sessionFixture(t)
	old, _ := loginForTest(t, service)
	ctx, started, release := pauseRefreshRead(t, repo.db)
	result := make(chan error, 1)
	go func() { _, _, _, err := service.RefreshAccessToken(ctx, old.RefreshToken); result <- err }()
	waitResult(t, started)
	current, _ := loginForTest(t, service)
	release()
	if err := waitResult(t, result); !errors.Is(err, apierror.ErrUnauthorized) {
		t.Fatalf("stale refresh updated new session: %v", err)
	}
	if _, err := validator.ValidateAccessToken(context.Background(), current.Token); err != nil {
		t.Fatalf("new login overwritten: %v", err)
	}
}

func TestOldLogoutDoesNotRevokeNewLogin(t *testing.T) {
	_, service, validator := sessionFixture(t)
	old, oldClaims := loginForTest(t, service)
	current, currentClaims := loginForTest(t, service)
	if oldClaims.SessionID == currentClaims.SessionID {
		t.Fatal("new login reused old session")
	}
	if err := service.Logout(context.Background(), old.AccountID, oldClaims.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := validator.ValidateAccessToken(context.Background(), current.Token); err != nil {
		t.Fatalf("old logout revoked new login: %v", err)
	}
}

func TestRefreshChecksDatabaseExpiryAtReadAndWrite(t *testing.T) {
	for _, pause := range []bool{false, true} {
		t.Run(map[bool]string{false: "expired-before-read", true: "expired-before-update"}[pause], func(t *testing.T) {
			repo, service, _ := sessionFixture(t)
			login, _ := loginForTest(t, service)
			ctx := context.Background()
			var release func()
			var result chan error
			if pause {
				var started <-chan struct{}
				ctx, started, release = pauseRefreshRead(t, repo.db)
				result = make(chan error, 1)
				go func() { _, _, _, err := service.RefreshAccessToken(ctx, login.RefreshToken); result <- err }()
				waitResult(t, started)
			}
			if err := repo.db.Model(&Account{}).Where("id = ?", login.AccountID).
				Update("refresh_expires_at", gorm.Expr("DATE_SUB(UTC_TIMESTAMP(6), INTERVAL 1 SECOND)")).Error; err != nil {
				t.Fatal(err)
			}
			var err error
			if pause {
				release()
				err = waitResult(t, result)
			} else {
				_, _, _, err = service.RefreshAccessToken(ctx, login.RefreshToken)
			}
			if !errors.Is(err, apierror.ErrUnauthorized) {
				t.Fatalf("expired refresh accepted: %v", err)
			}
		})
	}
}

func TestRenamePreservesSessionAndCannotRestoreRevokedLogin(t *testing.T) {
	repo, service, validator := sessionFixture(t)
	login, claims := loginForTest(t, service)
	ctx := context.Background()
	token, err := service.Rename(ctx, login.AccountID, claims.SessionID, "renamed")
	if err != nil {
		t.Fatal(err)
	}
	if current, err := validator.ValidateAccessToken(ctx, token); err != nil || current.SessionID != claims.SessionID || current.Username != "renamed" {
		t.Fatalf("invalid renamed session: %v", err)
	}
	if err := service.Logout(ctx, login.AccountID, claims.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Rename(ctx, login.AccountID, claims.SessionID, "should-not-change"); !errors.Is(err, apierror.ErrUnauthorized) {
		t.Fatalf("stale rename succeeded: %v", err)
	}
	current, err := repo.FindByID(ctx, login.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Username != "renamed" || current.Token != "" {
		t.Fatal("stale rename mutated a revoked session")
	}
}

func TestPasswordChangeRevokesSessionAndRejectsStaleLogin(t *testing.T) {
	repo, service, validator := sessionFixture(t)
	login, _ := loginForTest(t, service)
	ctx := context.Background()
	snapshot, err := repo.FindByID(ctx, login.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ChangePassword(ctx, "tester", "old-password", "new-password"); err != nil {
		t.Fatal(err)
	}
	if _, err := validator.ValidateAccessToken(ctx, login.Token); !errors.Is(err, apierror.ErrUnauthorized) {
		t.Fatal("password change did not revoke access")
	}
	if _, _, _, err := service.RefreshAccessToken(ctx, login.RefreshToken); !errors.Is(err, apierror.ErrUnauthorized) {
		t.Fatal("password change did not revoke refresh")
	}
	if _, err := repo.StartSession(ctx, snapshot, "stale-session", "stale-token", "stale-refresh"); !errors.Is(err, apierror.ErrUnauthorized) {
		t.Fatalf("login using old password snapshot succeeded: %v", err)
	}
	if _, err := service.Login(ctx, "tester", "old-password"); !errors.Is(err, apierror.ErrUnauthorized) {
		t.Fatal("old password accepted")
	}
	if _, err := service.Login(ctx, "tester", "new-password"); err != nil {
		t.Fatal(err)
	}
}

func TestSessionWriteFailuresArePropagated(t *testing.T) {
	for _, operation := range []string{"refresh", "logout", "change-password"} {
		t.Run(operation, func(t *testing.T) {
			repo, service, validator := sessionFixture(t)
			login, claims := loginForTest(t, service)
			revocations := 0
			service.onSessionRevoked = func(uint, string) { revocations++ }
			if err := repo.db.Callback().Update().Before("gorm:update").Register("test:fail_update", func(tx *gorm.DB) { tx.AddError(errors.New("injected database write failure")) }); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { repo.db.Callback().Update().Remove("test:fail_update") })
			var err error
			switch operation {
			case "refresh":
				_, _, _, err = service.RefreshAccessToken(context.Background(), login.RefreshToken)
			case "logout":
				err = service.Logout(context.Background(), login.AccountID, claims.SessionID)
			case "change-password":
				err = service.ChangePassword(context.Background(), "tester", "old-password", "new-password")
			}
			if !errors.Is(err, apierror.ErrServiceUnavailable) {
				t.Fatalf("write failure not propagated: %v", err)
			}
			if revocations != 0 {
				t.Fatal("revocation notification sent before successful commit")
			}
			if _, err := validator.ValidateAccessToken(context.Background(), login.Token); err != nil {
				t.Fatalf("failed operation modified session: %v", err)
			}
			if operation == "change-password" {
				current, err := repo.FindByID(context.Background(), login.AccountID)
				if err != nil {
					t.Fatal(err)
				}
				if err := bcrypt.CompareHashAndPassword([]byte(current.Password), []byte("old-password")); err != nil {
					t.Fatal("password change was not rolled back")
				}
			}
		})
	}
}

func TestExistingAccountMigrationRequiresLoginAndAddsRefreshIndex(t *testing.T) {
	t.Setenv("JWT_SECRET", "test-signing-key")
	db := testutil.NewMySQL(t)
	if err := db.Exec(`CREATE TABLE accounts (
		id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
		username VARCHAR(191) UNIQUE, password LONGTEXT, token LONGTEXT,
		refresh_token LONGTEXT, avatar_url VARCHAR(512), bio VARCHAR(255)
	)`).Error; err != nil {
		t.Fatal(err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte("password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	legacyRefresh, err := auth.GenerateRefreshToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO accounts (username,password,token,refresh_token) VALUES (?,?,?,?)",
		"legacy", string(hash), "legacy-access", legacyRefresh).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&Account{}); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasIndex(&Account{}, "idx_accounts_refresh_token") {
		t.Fatal("missing refresh-token lookup index")
	}
	repo := NewAccountRepository(db)
	service := NewAccountService(repo, nil)
	current, err := repo.FindByUsername(context.Background(), "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if current.SessionID != "" || current.RefreshExpiresAt != nil {
		t.Fatal("legacy account received an active session")
	}
	if _, _, _, err := service.RefreshAccessToken(context.Background(), legacyRefresh); !errors.Is(err, apierror.ErrUnauthorized) {
		t.Fatal("legacy refresh token bypassed reauthentication")
	}
	login, err := service.Login(context.Background(), "legacy", "password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewSessionValidator(repo).ValidateAccessToken(context.Background(), login.Token); err != nil {
		t.Fatal(err)
	}
}
