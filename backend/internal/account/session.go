package account

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"time"

	"feedsystem_video_go/internal/apierror"
	"feedsystem_video_go/internal/auth"

	"gorm.io/gorm"
)

type SessionReader interface {
	FindSession(context.Context, uint) (*Account, error)
}

type SessionValidator struct {
	repo SessionReader
}

func NewSessionValidator(repo SessionReader) *SessionValidator {
	return &SessionValidator{repo: repo}
}

func (v *SessionValidator) ValidateAccessToken(ctx context.Context, token string) (*auth.Claims, error) {
	claims, err := auth.ParseToken(token)
	if err != nil {
		return nil, apierror.ErrUnauthorized
	}
	// The repository uses the primary connection in a new, autocommit query.
	// Cached session state must never grant access or be written back here.
	lookupCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	account, err := v.repo.FindSession(lookupCtx, claims.AccountID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, apierror.ErrUnauthorized
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %w", apierror.ErrServiceUnavailable, err)
	}
	if account == nil || account.SessionID == "" || account.SessionID != claims.SessionID ||
		subtle.ConstantTimeCompare([]byte(account.Token), []byte(token)) != 1 {
		return nil, apierror.ErrUnauthorized
	}
	claims.Username = account.Username
	return claims, nil
}

var _ auth.TokenValidator = (*SessionValidator)(nil)
