package account

import (
	"context"

	"feedsystem_video_go/internal/apierror"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type AccountRepository struct {
	db *gorm.DB
}

func NewAccountRepository(db *gorm.DB) *AccountRepository {
	return &AccountRepository{db: db}
}

func (ar *AccountRepository) CreateAccount(ctx context.Context, account *Account) error {
	return ar.db.WithContext(ctx).Create(account).Error
}

func (ar *AccountRepository) FindByID(ctx context.Context, id uint) (*Account, error) {
	var account Account
	err := ar.db.WithContext(ctx).First(&account, id).Error
	return &account, err
}

func (ar *AccountRepository) FindByUsername(ctx context.Context, username string) (*Account, error) {
	var account Account
	err := ar.db.WithContext(ctx).Where("username = ?", username).First(&account).Error
	return &account, err
}

func (ar *AccountRepository) FindSession(ctx context.Context, id uint) (*Account, error) {
	var account Account
	err := ar.db.WithContext(ctx).Select("id", "username", "token", "session_id").First(&account, id).Error
	return &account, err
}

// StartSession serializes session replacement with password changes and returns
// the exact superseded session, so delayed notifications cannot close a new one.
func (ar *AccountRepository) StartSession(ctx context.Context, snapshot *Account, sessionID, token, refreshToken string) (string, error) {
	var previousSession string
	err := ar.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current Account
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Select("id", "username", "password", "session_id").First(&current, snapshot.ID).Error; err != nil {
			return err
		}
		if current.Password != snapshot.Password || current.Username != snapshot.Username {
			return apierror.ErrUnauthorized
		}
		previousSession = current.SessionID
		return requireSessionUpdate(tx.Model(&Account{}).Where("id = ?", current.ID).
			Updates(map[string]any{
				"session_id":         sessionID,
				"token":              token,
				"refresh_token":      refreshToken,
				"refresh_expires_at": gorm.Expr("DATE_ADD(UTC_TIMESTAMP(6), INTERVAL ? SECOND)", int64(RefreshTokenTTL.Seconds())),
			}))
	})
	return previousSession, err
}

func revokedSessionFields() map[string]any {
	return map[string]any{
		"session_id": "", "token": "", "refresh_token": "", "refresh_expires_at": nil,
	}
}

func (ar *AccountRepository) RevokeSession(ctx context.Context, id uint, sessionID string) error {
	if id == 0 || sessionID == "" {
		return apierror.ErrUnauthorized
	}
	// Zero affected rows are an idempotent success: a newer session is untouched.
	return ar.db.WithContext(ctx).Model(&Account{}).
		Where("id = ? AND session_id = ?", id, sessionID).
		Updates(revokedSessionFields()).Error
}

func (ar *AccountRepository) RenameSession(ctx context.Context, id uint, sessionID, username, token string) error {
	if sessionID == "" {
		return apierror.ErrUnauthorized
	}
	return requireSessionUpdate(ar.db.WithContext(ctx).Model(&Account{}).
		Where("id = ? AND session_id = ?", id, sessionID).
		Updates(map[string]any{"username": username, "token": token}))
}

func (ar *AccountRepository) ChangePasswordAndRevoke(ctx context.Context, snapshot *Account, passwordHash string) (string, error) {
	var previousSession string
	err := ar.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current Account
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Select("id", "password", "session_id").First(&current, snapshot.ID).Error; err != nil {
			return err
		}
		if current.Password != snapshot.Password {
			return apierror.ErrUnauthorized
		}
		previousSession = current.SessionID
		fields := revokedSessionFields()
		fields["password"] = passwordHash
		return tx.Model(&Account{}).Where("id = ?", current.ID).Updates(fields).Error
	})
	return previousSession, err
}

func (ar *AccountRepository) FindByRefreshToken(ctx context.Context, token string) (*Account, error) {
	var account Account
	err := ar.db.WithContext(ctx).
		Where("refresh_token = ? AND session_id <> '' AND token <> '' AND refresh_expires_at > UTC_TIMESTAMP(6)", token).
		First(&account).Error
	return &account, err
}

// RefreshSession rechecks the session under a row lock. The token callback sees
// the latest username and access token, including a concurrent rename/refresh.
// Keep token selection inside the transaction so API instances share one result.
func (ar *AccountRepository) RefreshSession(ctx context.Context, snapshot *Account, tokenForCurrent func(*Account) (string, error)) (*Account, error) {
	var current Account
	err := ar.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Select("id", "username", "session_id", "token", "refresh_token").
			Where("id = ? AND session_id = ? AND refresh_token = ? AND token <> '' AND refresh_expires_at > UTC_TIMESTAMP(6)",
				snapshot.ID, snapshot.SessionID, snapshot.RefreshToken).
			First(&current).Error; err != nil {
			return err
		}
		token, err := tokenForCurrent(&current)
		if err != nil {
			return err
		}
		if token == current.Token {
			return nil
		}
		if err := requireSessionUpdate(tx.Model(&Account{}).
			Where("id = ? AND session_id = ? AND refresh_token = ? AND refresh_expires_at > UTC_TIMESTAMP(6)",
				current.ID, current.SessionID, current.RefreshToken).
			Update("token", token)); err != nil {
			return err
		}
		current.Token = token
		return nil
	})
	return &current, err
}

func requireSessionUpdate(result *gorm.DB) error {
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return apierror.ErrUnauthorized
	}
	return nil
}

func (ar *AccountRepository) UpdateAvatar(ctx context.Context, accountID uint, avatarURL string) error {
	return ar.db.WithContext(ctx).Model(&Account{}).Where("id = ?", accountID).Update("avatar_url", avatarURL).Error
}

func (ar *AccountRepository) UpdateFields(ctx context.Context, id uint, updates map[string]interface{}) error {
	return ar.db.WithContext(ctx).Model(&Account{}).Where("id = ?", id).Updates(updates).Error
}
