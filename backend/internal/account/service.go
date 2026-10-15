package account

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"strings"
	"time"

	"feedsystem_video_go/internal/apierror"
	"feedsystem_video_go/internal/auth"

	"github.com/go-sql-driver/mysql"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

const RefreshTokenTTL = 7 * 24 * time.Hour

// Avoid returning a token that will expire while the caller retries its request.
const refreshReuseMinTTL = time.Minute

type AccountService struct {
	accountRepository *AccountRepository
	onSessionRevoked  func(uint, string)
}

var (
	ErrUsernameTaken       = errors.New("username already exists")
	ErrNewUsernameRequired = fmt.Errorf("%w: new_username is required", apierror.ErrValidation)
)

func NewAccountService(repo *AccountRepository, onSessionRevoked func(uint, string)) *AccountService {
	return &AccountService{accountRepository: repo, onSessionRevoked: onSessionRevoked}
}

func (as *AccountService) notifyRevoked(accountID uint, sessionID string) {
	if sessionID != "" && as.onSessionRevoked != nil {
		as.onSessionRevoked(accountID, sessionID)
	}
}

func sessionStoreError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, gorm.ErrRecordNotFound) || errors.Is(err, apierror.ErrUnauthorized) {
		return apierror.ErrUnauthorized
	}
	return fmt.Errorf("%w: %w", apierror.ErrServiceUnavailable, err)
}

func (as *AccountService) CreateAccount(ctx context.Context, account *Account) error {
	passwordHash, err := bcrypt.GenerateFromPassword([]byte(account.Password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	account.Password = string(passwordHash)
	return as.accountRepository.CreateAccount(ctx, account)
}

func (as *AccountService) Login(ctx context.Context, username, password string) (LoginResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	account, err := as.accountRepository.FindByUsername(ctx, username)
	if err != nil {
		return LoginResponse{}, sessionStoreError(err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(account.Password), []byte(password)); err != nil {
		return LoginResponse{}, apierror.ErrUnauthorized
	}
	sessionID, err := auth.GenerateSessionID()
	if err != nil {
		return LoginResponse{}, err
	}
	token, err := auth.GenerateToken(account.ID, account.Username, sessionID)
	if err != nil {
		return LoginResponse{}, err
	}
	refreshToken, err := auth.GenerateRefreshToken()
	if err != nil {
		return LoginResponse{}, err
	}
	previousSession, err := as.accountRepository.StartSession(ctx, account, sessionID, token, refreshToken)
	if err != nil {
		return LoginResponse{}, sessionStoreError(err)
	}
	as.notifyRevoked(account.ID, previousSession)
	return LoginResponse{Token: token, RefreshToken: refreshToken, AccountID: account.ID, Username: account.Username}, nil
}

func (as *AccountService) Logout(ctx context.Context, accountID uint, sessionID string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := as.accountRepository.RevokeSession(ctx, accountID, sessionID); err != nil {
		return sessionStoreError(err)
	}
	as.notifyRevoked(accountID, sessionID)
	return nil
}

func (as *AccountService) Rename(ctx context.Context, accountID uint, sessionID, newUsername string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	newUsername = strings.TrimSpace(newUsername)
	if newUsername == "" {
		return "", ErrNewUsernameRequired
	}
	if sessionID == "" {
		return "", apierror.ErrUnauthorized
	}
	token, err := auth.GenerateToken(accountID, newUsername, sessionID)
	if err != nil {
		return "", err
	}
	if err := as.accountRepository.RenameSession(ctx, accountID, sessionID, newUsername, token); err != nil {
		var mysqlErr *mysql.MySQLError
		if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
			return "", ErrUsernameTaken
		}
		return "", sessionStoreError(err)
	}
	return token, nil
}

func (as *AccountService) ChangePassword(ctx context.Context, username, oldPassword, newPassword string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	account, err := as.accountRepository.FindByUsername(ctx, username)
	if err != nil {
		return sessionStoreError(err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(account.Password), []byte(oldPassword)); err != nil {
		return apierror.ErrUnauthorized
	}
	passwordHash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("%w: invalid new password", apierror.ErrValidation)
	}
	previousSession, err := as.accountRepository.ChangePasswordAndRevoke(ctx, account, string(passwordHash))
	if err != nil {
		return sessionStoreError(err)
	}
	as.notifyRevoked(account.ID, previousSession)
	return nil
}

func (as *AccountService) RefreshAccessToken(ctx context.Context, refreshToken string) (string, uint, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if len(refreshToken) != 64 {
		return "", 0, "", apierror.ErrUnauthorized
	}
	account, err := as.accountRepository.FindByRefreshToken(ctx, refreshToken)
	if err != nil {
		return "", 0, "", sessionStoreError(err)
	}
	if subtle.ConstantTimeCompare([]byte(account.RefreshToken), []byte(refreshToken)) != 1 {
		return "", 0, "", apierror.ErrUnauthorized
	}
	var tokenErr error
	current, err := as.accountRepository.RefreshSession(ctx, account, func(current *Account) (string, error) {
		claims, err := auth.ParseToken(current.Token)
		if err == nil && claims.AccountID == current.ID && claims.SessionID == current.SessionID &&
			claims.Username == current.Username && time.Until(claims.ExpiresAt.Time) > refreshReuseMinTTL {
			return current.Token, nil
		}
		// A newly issued token is reused by the next refresh, even when requests
		// come from different browser tabs or different API instances.
		token, err := auth.GenerateToken(current.ID, current.Username, current.SessionID)
		tokenErr = err
		return token, err
	})
	if tokenErr != nil {
		return "", 0, "", tokenErr
	}
	if err != nil {
		return "", 0, "", sessionStoreError(err)
	}
	return current.Token, current.ID, current.Username, nil
}

func (as *AccountService) FindByID(ctx context.Context, id uint) (*Account, error) {
	return as.accountRepository.FindByID(ctx, id)
}

func (as *AccountService) FindByUsername(ctx context.Context, username string) (*Account, error) {
	return as.accountRepository.FindByUsername(ctx, username)
}

func (as *AccountService) UpdateAvatar(ctx context.Context, accountID uint, avatarURL string) error {
	return as.accountRepository.UpdateAvatar(ctx, accountID, avatarURL)
}

func (as *AccountService) UpdateProfile(ctx context.Context, accountID uint, req *UpdateProfileRequest) error {
	updates := map[string]interface{}{}
	if req.Bio != "" {
		updates["bio"] = strings.TrimSpace(req.Bio)
	}
	if req.AvatarURL != "" {
		updates["avatar_url"] = strings.TrimSpace(req.AvatarURL)
	}
	if len(updates) == 0 {
		return apierror.ErrValidation
	}
	return as.accountRepository.UpdateFields(ctx, accountID, updates)
}
