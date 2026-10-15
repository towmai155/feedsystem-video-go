// internal/auth/jwt.go
package auth

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	"os"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const AccessTokenTTL = 15 * time.Minute

// Keep the development fallback stable for this process, including signing and
// verification. Deployments with multiple API instances must set JWT_SECRET.
var fallbackSecret = sync.OnceValues(func() ([]byte, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	log.Print("JWT_SECRET not set; using a process-local development key")
	return b, nil
})

func jwtSecret() ([]byte, error) {
	secret := os.Getenv("JWT_SECRET")
	if secret != "" {
		return []byte(secret), nil
	}
	return fallbackSecret()
}

type Claims struct {
	AccountID uint   `json:"account_id"`
	Username  string `json:"username"`
	SessionID string `json:"sid"`
	jwt.RegisteredClaims
}

func GenerateToken(accountID uint, username, sessionID string) (string, error) {
	if accountID == 0 || sessionID == "" {
		return "", errors.New("account ID and session ID are required")
	}
	key, err := jwtSecret()
	if err != nil {
		return "", err
	}
	tokenID, err := GenerateSessionID()
	if err != nil {
		return "", err
	}
	now := time.Now()

	claims := Claims{
		AccountID: accountID,
		Username:  username,
		SessionID: sessionID,
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        tokenID,
			ExpiresAt: jwt.NewNumericDate(now.Add(AccessTokenTTL)),
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)

	return token.SignedString(key)
}

func randomToken(size int) (string, error) {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func GenerateSessionID() (string, error) {
	return randomToken(16)
}

func GenerateRefreshToken() (string, error) {
	return randomToken(32)
}

func ParseToken(tokenString string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(
		tokenString,
		&Claims{},
		func(token *jwt.Token) (interface{}, error) {
			if token.Method == nil || token.Method.Alg() != jwt.SigningMethodHS256.Alg() {
				return nil, errors.New("unexpected signing method")
			}
			return jwtSecret()
		},
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return nil, err
	}

	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid || claims.AccountID == 0 || claims.SessionID == "" || claims.ID == "" {
		return nil, jwt.ErrTokenInvalidClaims
	}

	return claims, nil
}
