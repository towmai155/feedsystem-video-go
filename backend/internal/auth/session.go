package auth

import "context"

// TokenValidator is shared by HTTP endpoints and long-lived SSE connections.
type TokenValidator interface {
	ValidateAccessToken(context.Context, string) (*Claims, error)
}
