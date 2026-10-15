package auth

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestAccessTokenHasSessionAndUniqueID(t *testing.T) {
	t.Setenv("JWT_SECRET", "test-signing-key")
	first, err := GenerateToken(7, "tester", "session-one")
	if err != nil {
		t.Fatal(err)
	}
	second, err := GenerateToken(7, "tester", "session-one")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("tokens issued together must have distinct IDs")
	}
	claims, err := ParseToken(first)
	if err != nil {
		t.Fatal(err)
	}
	if claims.AccountID != 7 || claims.SessionID != "session-one" || claims.ID == "" {
		t.Fatal("missing account, session or token ID")
	}
	if lifetime := claims.ExpiresAt.Sub(claims.IssuedAt.Time); lifetime != AccessTokenTTL {
		t.Fatalf("unexpected token lifetime: %v", lifetime)
	}
}

func TestRejectExpiredLegacyAndUnsignedClaims(t *testing.T) {
	t.Setenv("JWT_SECRET", "test-signing-key")
	for _, name := range []string{"expired", "missing-expiry", "legacy-session", "missing-jti", "wrong-algorithm"} {
		t.Run(name, func(t *testing.T) {
			claims := Claims{AccountID: 7, SessionID: "session-one", RegisteredClaims: jwt.RegisteredClaims{
				ID: "token-id", ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Minute)),
			}}
			method := jwt.SigningMethodHS256
			switch name {
			case "expired":
				claims.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Minute))
			case "missing-expiry":
				claims.ExpiresAt = nil
			case "legacy-session":
				claims.SessionID = ""
			case "missing-jti":
				claims.ID = ""
			case "wrong-algorithm":
				method = jwt.SigningMethodHS384
			}
			token, err := jwt.NewWithClaims(method, claims).SignedString([]byte("test-signing-key"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ParseToken(token); err == nil {
				t.Fatal("invalid token accepted")
			}
		})
	}
}

func TestDevelopmentSigningKeyIsStableWithinProcess(t *testing.T) {
	t.Setenv("JWT_SECRET", "")
	token, err := GenerateToken(7, "tester", "session-one")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseToken(token); err != nil {
		t.Fatalf("cannot verify generated token: %v", err)
	}
}
