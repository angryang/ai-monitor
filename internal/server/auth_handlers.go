// Package server: /auth/* handlers (login, logout, me, change-password).
// Handlers are intentionally small: the heavy lifting (JWT, bcrypt) lives
// in internal/auth and is wired in through the *auth.Middleware.
package server

import (
	"errors"
	"net/http"
	"time"

	"github.com/angryang/ai-monitor/internal/auth"
)

// AuthHandlersConfig is the dependency bundle the handlers need. It's a
// copy of the fields we already expose on Options so the type system can
// refuse construction when auth is disabled (otherwise the handlers would
// nil-deref).
type AuthHandlersConfig struct {
	Auth   *auth.Middleware
	Users  auth.UserStore
	Logger LoggerFunc
}

// AuthHandlers holds the routes + state for /auth/*.
type AuthHandlers struct {
	cfg AuthHandlersConfig
}

// NewAuthHandlers returns a handlers bundle.
func NewAuthHandlers(cfg *AuthHandlersConfig) *AuthHandlers {
	if cfg == nil {
		cfg = &AuthHandlersConfig{}
	}
	return &AuthHandlers{cfg: *cfg}
}

// Routes returns a fresh ServeMux containing every /auth/* route.
func (h *AuthHandlers) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /auth/login", h.handleLogin())
	mux.HandleFunc("POST /auth/logout", h.handleLogout())
	mux.HandleFunc("GET /auth/me", h.requireAuth(h.handleMe()))
	mux.HandleFunc("POST /auth/change-password", h.requireAuth(h.handleChangePassword()))
	return mux
}

// ---- handlers ----

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type loginResponse struct {
	Token     string       `json:"token"`
	User      auth.UserView `json:"user"`
	ExpiresAt time.Time    `json:"expires_at"`
}

func (h *AuthHandlers) handleLogin() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.cfg.Auth == nil || h.cfg.Users == nil {
			WriteAPIError(w, http.StatusInternalServerError, "auth_not_configured", "auth is not wired")
			return
		}
		var req loginRequest
		if apiErr := readJSON(r, &req); apiErr != nil {
			WriteAPIError(w, apiErr.Status, apiErr.Code, apiErr.Message)
			return
		}
		if req.Username == "" || req.Password == "" {
			WriteAPIError(w, http.StatusBadRequest, "missing_field", "username and password required")
			return
		}
		u, err := h.cfg.Users.GetByUsername(r.Context(), req.Username)
		if err != nil {
			if errors.Is(err, auth.ErrUserNotFound) {
				WriteAPIError(w, http.StatusUnauthorized, "invalid_credentials", "invalid credentials")
				return
			}
			WriteAPIError(w, http.StatusInternalServerError, "lookup_failed", err.Error())
			return
		}
		if !u.Enabled {
			WriteAPIError(w, http.StatusUnauthorized, "user_disabled", "account disabled")
			return
		}
		hasher := auth.NewBcryptHasher()
		if !hasher.Verify(u.PasswordHash, req.Password) {
			WriteAPIError(w, http.StatusUnauthorized, "invalid_credentials", "invalid credentials")
			return
		}
		claims := auth.Claims{UserID: u.ID, Username: u.Username, Role: string(u.Role)}
		tok, err := h.cfg.Auth.Signer().Sign(&claims)
		if err != nil {
			WriteAPIError(w, http.StatusInternalServerError, "sign_failed", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, loginResponse{
			Token:     tok,
			User:      u.PublicView(),
			ExpiresAt: claims.ExpiresAt.Time,
		})
	}
}

// handleLogout is intentionally a no-op on the server side — JWTs are
// stateless, so all the client has to do is delete the token. We still
// return 200 so the SPA can complete its fetch cleanly.
func (h *AuthHandlers) handleLogout() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}
}

// handleMe returns the currently authenticated user. The middleware has
// already validated the token by the time we get here.
func (h *AuthHandlers) handleMe() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, ok := auth.UserFromContext(r.Context())
		if !ok {
			WriteAPIError(w, http.StatusInternalServerError, "missing_user", "no user in context")
			return
		}
		writeJSON(w, http.StatusOK, u.PublicView())
	}
}

// changePasswordRequest is the body for POST /auth/change-password.
type changePasswordRequest struct {
	OldPassword string `json:"old_password"`
	NewPassword string `json:"new_password"`
}

func (h *AuthHandlers) handleChangePassword() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, ok := auth.UserFromContext(r.Context())
		if !ok {
			WriteAPIError(w, http.StatusInternalServerError, "missing_user", "no user in context")
			return
		}
		var req changePasswordRequest
		if apiErr := readJSON(r, &req); apiErr != nil {
			WriteAPIError(w, apiErr.Status, apiErr.Code, apiErr.Message)
			return
		}
		if req.OldPassword == "" || req.NewPassword == "" {
			WriteAPIError(w, http.StatusBadRequest, "missing_field", "old_password and new_password required")
			return
		}
		if err := h.cfg.Users.ChangePassword(r.Context(), u.ID, req.OldPassword, req.NewPassword); err != nil {
			if errors.Is(err, auth.ErrPasswordMismatch) {
				WriteAPIError(w, http.StatusUnprocessableEntity, "old_password_wrong", "old password does not match")
				return
			}
			if errors.Is(err, auth.ErrPasswordTooShort) {
				WriteAPIError(w, http.StatusUnprocessableEntity, "password_too_short", err.Error())
				return
			}
			WriteAPIError(w, http.StatusInternalServerError, "change_failed", err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}
}

// requireAuth wraps the supplied handler in the auth middleware's
// Authenticate decorator so handlers don't have to remember to repeat it.
func (h *AuthHandlers) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.cfg.Auth == nil {
			WriteAPIError(w, http.StatusInternalServerError, "auth_not_configured", "auth missing")
			return
		}
		h.cfg.Auth.Authenticate(next).ServeHTTP(w, r)
	}
}