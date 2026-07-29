package middleware

import (
	"strings"

	"github.com/brickster241/PulseHTTP/httpcore"
)

// Role is an ordered privilege level.
type Role int

const (
	RoleAnonymous Role = iota
	RoleUser
	RoleAdmin
)

// AuthConfig maps bearer tokens to roles and paths to required roles.
//
// The middleware is strict about the 401/403 boundary:
//
//	no credentials or an unknown token  -> 401 + WWW-Authenticate (authenticate first)
//	valid token, insufficient role      -> 403 (authenticated, still denied)
type AuthConfig struct {
	Tokens map[string]Role // bearer token -> role
	// RequiredRole returns the role a path needs. Paths needing
	// RoleAnonymous skip auth entirely.
	RequiredRole func(path string) Role
}

// BearerAuth enforces AuthConfig.
func BearerAuth(cfg AuthConfig) Middleware {
	return func(next httpcore.Handler) httpcore.Handler {
		return func(req *httpcore.Request, w *httpcore.ResponseWriter) {
			need := cfg.RequiredRole(req.Path)
			if need == RoleAnonymous {
				next(req, w)
				return
			}
			raw := req.Headers.Get("Authorization")
			token, isBearer := strings.CutPrefix(raw, "Bearer ")
			if raw == "" || !isBearer {
				w.Header().Set("WWW-Authenticate", `Bearer realm="pulse"`)
				w.Error(httpcore.StatusUnauthorized, "missing or malformed bearer credentials")
				return
			}
			role, known := cfg.Tokens[strings.TrimSpace(token)]
			if !known {
				w.Header().Set("WWW-Authenticate", `Bearer realm="pulse", error="invalid_token"`)
				w.Error(httpcore.StatusUnauthorized, "unknown token")
				return
			}
			if role < need {
				// Authenticated but not privileged: this is 403, never 401.
				w.Error(httpcore.StatusForbidden, "insufficient privileges for "+req.Path)
				return
			}
			req.Meta["role"] = role
			next(req, w)
		}
	}
}
