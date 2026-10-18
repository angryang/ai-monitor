package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// ctxKey is unexported so external callers can't collide with our keys.
type ctxKey string

const (
	// CtxClaimsKey is the context key under which authenticated *Claims are
	// stored. Handlers retrieve them via ClaimsFromContext.
	CtxClaimsKey ctxKey = "auth.claims"
	// CtxUserKey is the context key under which the resolved *User is
	// stored after Authenticate. We separate it from claims so middleware
	// can refresh a stale user without touching the JWT.
	CtxUserKey ctxKey = "auth.user"
)

// ClaimsFromContext returns the JWT claims attached by Authenticate.
func ClaimsFromContext(ctx context.Context) (*Claims, bool) {
	c, found := ctx.Value(CtxClaimsKey).(*Claims)
	return c, found
}

// UserFromContext returns the user attached by Authenticate.
func UserFromContext(ctx context.Context) (*User, bool) {
	u, found := ctx.Value(CtxUserKey).(*User)
	return u, found
}

// WithClaims injects claims into ctx — used by tests that want to fake
// authentication without exercising the middleware.
func WithClaims(ctx context.Context, c *Claims, u *User) context.Context {
	ctx = context.WithValue(ctx, CtxClaimsKey, c)
	ctx = context.WithValue(ctx, CtxUserKey, u)
	return ctx
}

// Middleware glues the signer and the user store into a small request
// pipeline. It exposes two decorators: Authenticate (verify the bearer
// token) and RequireRole (gate by role). They're separate so handlers can
// accept any logged-in user but then refuse viewers on /admin/* routes.
type Middleware struct {
	signer TokenSigner
	users  UserStore
}

// NewMiddleware constructs the bundle. Either dep can be nil for tests
// that only exercise one half.
func NewMiddleware(signer TokenSigner, users UserStore) *Middleware {
	return &Middleware{signer: signer, users: users}
}

// Signer exposes the underlying TokenSigner so login handlers can mint
// tokens without duplicating the constructor wiring.
func (m *Middleware) Signer() TokenSigner {
	if m == nil {
		return nil
	}
	return m.signer
}

// Users exposes the UserStore. Used by admin handlers that need full CRUD.
func (m *Middleware) Users() UserStore {
	if m == nil {
		return nil
	}
	return m.users
}

// Authenticate parses the bearer token and resolves the user from the DB.
// On failure it writes a 401 (or 403 for disabled accounts) and returns
// without calling next.
//
// The handler is the only public mutator of the response, so it's the only
// thing that needs to know about our JSON envelope. We deliberately don't
// take a logger here — the calling layer wraps the chain.
func (m *Middleware) Authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, ok := extractBearer(r)
		if !ok {
			writeAuthError(w, http.StatusUnauthorized, "missing_token", "bearer token required")
			return
		}
		claims, err := m.signer.Parse(raw)
		if err != nil {
			switch {
			case errors.Is(err, ErrTokenExpired):
				writeAuthError(w, http.StatusUnauthorized, "token_expired", err.Error())
			default:
				writeAuthError(w, http.StatusUnauthorized, "invalid_token", err.Error())
			}
			return
		}
		user, err := m.users.GetByID(r.Context(), claims.UserID)
		if err != nil {
			if errors.Is(err, ErrUserNotFound) {
				writeAuthError(w, http.StatusUnauthorized, "user_not_found", "user no longer exists")
				return
			}
			writeAuthError(w, http.StatusInternalServerError, "lookup_failed", err.Error())
			return
		}
		if !user.Enabled {
			writeAuthError(w, http.StatusForbidden, "user_disabled", "account disabled")
			return
		}
		if user.Role != Role(claims.Role) {
			// role drift (admin → viewer etc); force re-login
			writeAuthError(w, http.StatusForbidden, "role_drift", "role changed; please re-login")
			return
		}
		ctx := WithClaims(r.Context(), claims, &user)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequireRole wraps a handler with Authenticate + role gating. Callers
// don't need to nest Authenticate themselves; this keeps the call sites
// in admin handlers short and prevents the easy-to-miss mistake of
// forgetting to authenticate before role-checking.
func (m *Middleware) RequireRole(min Role, next http.Handler) http.Handler {
	return m.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, ok := ClaimsFromContext(r.Context())
		if !ok {
			// Defensive: Authenticate should already have written the 401.
			writeAuthError(w, http.StatusUnauthorized, "missing_token", "authentication required")
			return
		}
		if Role(claims.Role) != min {
			writeAuthError(w, http.StatusForbidden, "forbidden", fmt.Sprintf("role %q required", min))
			return
		}
		next.ServeHTTP(w, r)
	}))
}

// extractBearer pulls the token from the Authorization header. We accept
// only the canonical "Bearer X" form; cookies / query strings are out of
// scope for v1 (SPA stores the JWT in localStorage).
func extractBearer(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", false
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return "", false
	}
	return strings.TrimSpace(h[len(prefix):]), true
}

// writeAuthError is the single source of truth for JSON error envelopes
// emitted by middleware. Tests rely on the exact field names.
func writeAuthError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"error":{"code":%q,"message":%q}}`, code, msg)
}