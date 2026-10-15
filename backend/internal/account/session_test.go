package account

import (
	"context"
	"errors"
	"testing"

	"feedsystem_video_go/internal/apierror"
	"feedsystem_video_go/internal/auth"
	"gorm.io/gorm"
)

type sessionReaderFunc func(context.Context, uint) (*Account, error)

func (f sessionReaderFunc) FindSession(ctx context.Context, id uint) (*Account, error) {
	return f(ctx, id)
}

func TestSessionValidatorChecksAuthoritativeStateOnEveryRequest(t *testing.T) {
	t.Setenv("JWT_SECRET", "test-signing-key")
	token, err := auth.GenerateToken(7, "old-name", "session-one")
	if err != nil {
		t.Fatal(err)
	}
	current := &Account{ID: 7, Username: "current-name", SessionID: "session-one", Token: token}
	reads := 0
	validator := NewSessionValidator(sessionReaderFunc(func(ctx context.Context, id uint) (*Account, error) {
		reads++
		if id != 7 {
			t.Fatalf("unexpected account ID: %d", id)
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("session lookup needs a deadline")
		}
		return current, nil
	}))
	claims, err := validator.ValidateAccessToken(context.Background(), token)
	if err != nil || claims.Username != "current-name" {
		t.Fatalf("active session rejected: %v", err)
	}
	current = &Account{ID: 7}
	if _, err := validator.ValidateAccessToken(context.Background(), token); !errors.Is(err, apierror.ErrUnauthorized) {
		t.Fatalf("revoked session accepted: %v", err)
	}
	if reads != 2 {
		t.Fatalf("expected a database lookup on both requests; got %d", reads)
	}
}

func TestSessionValidatorDistinguishesInvalidSessionAndStoreFailure(t *testing.T) {
	t.Setenv("JWT_SECRET", "test-signing-key")
	token, err := auth.GenerateToken(7, "tester", "session-one")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name               string
		account            *Account
		storeErr, expected error
	}{
		{"not-found", nil, gorm.ErrRecordNotFound, apierror.ErrUnauthorized},
		{"replaced-session", &Account{SessionID: "session-two", Token: token}, nil, apierror.ErrUnauthorized},
		{"replaced-token", &Account{SessionID: "session-one", Token: "different"}, nil, apierror.ErrUnauthorized},
		{"database-unavailable", nil, errors.New("connection failed"), apierror.ErrServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			validator := NewSessionValidator(sessionReaderFunc(func(context.Context, uint) (*Account, error) { return tc.account, tc.storeErr }))
			if _, err := validator.ValidateAccessToken(context.Background(), token); !errors.Is(err, tc.expected) {
				t.Fatalf("expected %v, got %v", tc.expected, err)
			}
		})
	}
}
