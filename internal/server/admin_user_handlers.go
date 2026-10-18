// Package server: /admin/users handlers — full CRUD with role-aware
// visibility. Viewers always get 403; admins get full control except for
// the "can't delete yourself" + "can't delete last admin" safety rails.
package server

import (
	"errors"
	"net/http"

	"github.com/angryang/ai-monitor/internal/auth"
)

// AdminUserHandlersConfig carries the dependencies for admin/user routes.
type AdminUserHandlersConfig struct {
	Auth   *auth.Middleware
	Users  auth.UserStore
	Logger LoggerFunc
}

// AdminUserHandlers is the bundle returned by NewAdminUserHandlers.
type AdminUserHandlers struct {
	cfg AdminUserHandlersConfig
}

// compile-time assertions: keep these helpers local
var _ = http.MethodGet

// NewAdminUserHandlers wires the bundle.
func NewAdminUserHandlers(cfg *AdminUserHandlersConfig) *AdminUserHandlers {
	if cfg == nil {
		cfg = &AdminUserHandlersConfig{}
	}
	return &AdminUserHandlers{cfg: *cfg}
}

// Routes returns a mux covering /admin/users and /admin/users/{id}.
func (h *AdminUserHandlers) Routes() http.Handler {
	mux := http.NewServeMux()
	authAdmin := h.cfg.Auth.RequireRole(auth.RoleAdmin, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			h.list(w, r)
		case http.MethodPost:
			h.create(w, r)
		default:
			WriteAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", r.Method)
		}
	}))
	mux.Handle("/admin/users", authAdmin)

	idAuth := h.cfg.Auth.RequireRole(auth.RoleAdmin, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			h.get(w, r)
		case http.MethodPatch:
			h.patch(w, r)
		case http.MethodDelete:
			h.delete(w, r)
		default:
			WriteAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", r.Method)
		}
	}))
	mux.Handle("/admin/users/", idAuth)
	return mux
}

func (h *AdminUserHandlers) list(w http.ResponseWriter, r *http.Request) {
	users, err := h.cfg.Users.List(r.Context())
	if err != nil {
		WriteAPIError(w, http.StatusInternalServerError, "list_failed", err.Error())
		return
	}
	views := make([]auth.UserView, 0, len(users))
	for _, u := range users {
		views = append(views, u.PublicView())
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": views})
}

func (h *AdminUserHandlers) get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	u, err := h.cfg.Users.GetByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			WriteAPIError(w, http.StatusNotFound, "user_not_found", id)
			return
		}
		WriteAPIError(w, http.StatusInternalServerError, "get_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, u.PublicView())
}

type createUserRequest struct {
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Password    string `json:"password"`
	Role        string `json:"role"`
	Enabled     *bool  `json:"enabled,omitempty"`
}

func (h *AdminUserHandlers) create(w http.ResponseWriter, r *http.Request) {
	var req createUserRequest
	if apiErr := readJSON(r, &req); apiErr != nil {
		WriteAPIError(w, apiErr.Status, apiErr.Code, apiErr.Message)
		return
	}
	if req.Username == "" || req.Password == "" {
		WriteAPIError(w, http.StatusBadRequest, "missing_field", "username and password required")
		return
	}
	role, err := auth.ParseRole(req.Role)
	if err != nil {
		WriteAPIError(w, http.StatusBadRequest, "invalid_role", err.Error())
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	u, err := h.cfg.Users.Create(r.Context(), auth.CreateUserInput{
		Username:    req.Username,
		DisplayName: req.DisplayName,
		Password:    req.Password,
		Role:        role,
		Enabled:     enabled,
	})
	if err != nil {
		if errors.Is(err, auth.ErrUserAlreadyExists) {
			WriteAPIError(w, http.StatusConflict, "user_exists", err.Error())
			return
		}
		if errors.Is(err, auth.ErrPasswordTooShort) {
			WriteAPIError(w, http.StatusUnprocessableEntity, "password_too_short", err.Error())
			return
		}
		WriteAPIError(w, http.StatusInternalServerError, "create_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, u.PublicView())
}

type updateUserRequest struct {
	DisplayName *string `json:"display_name,omitempty"`
	Password    *string `json:"password,omitempty"`
	Role        *string `json:"role,omitempty"`
	Enabled     *bool   `json:"enabled,omitempty"`
}

func (h *AdminUserHandlers) patch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// Don't let admins disable / delete themselves; we read the caller id
	// off the request context.
	caller, _ := auth.UserFromContext(r.Context())
	if caller != nil && caller.ID == id {
		WriteAPIError(w, http.StatusUnprocessableEntity, "self_modify", "use /auth/change-password for self edits")
		return
	}

	var req updateUserRequest
	if apiErr := readJSON(r, &req); apiErr != nil {
		WriteAPIError(w, apiErr.Status, apiErr.Code, apiErr.Message)
		return
	}
	in := auth.UpdateUserInput{
		DisplayName: req.DisplayName,
		Password:    req.Password,
		Enabled:     req.Enabled,
	}
	if req.Role != nil {
		role, err := auth.ParseRole(*req.Role)
		if err != nil {
			WriteAPIError(w, http.StatusBadRequest, "invalid_role", err.Error())
			return
		}
		in.Role = &role
	}
	u, err := h.cfg.Users.Update(r.Context(), id, in)
	if err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			WriteAPIError(w, http.StatusNotFound, "user_not_found", id)
			return
		}
		if errors.Is(err, auth.ErrLastAdminProtected) {
			WriteAPIError(w, http.StatusUnprocessableEntity, "last_admin_protected", "cannot modify last admin")
			return
		}
		if errors.Is(err, auth.ErrPasswordTooShort) {
			WriteAPIError(w, http.StatusUnprocessableEntity, "password_too_short", err.Error())
			return
		}
		WriteAPIError(w, http.StatusInternalServerError, "update_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, u.PublicView())
}

func (h *AdminUserHandlers) delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	caller, _ := auth.UserFromContext(r.Context())
	if caller != nil && caller.ID == id {
		WriteAPIError(w, http.StatusUnprocessableEntity, "self_delete", "cannot delete yourself")
		return
	}
	if err := h.cfg.Users.Delete(r.Context(), id); err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			WriteAPIError(w, http.StatusNotFound, "user_not_found", id)
			return
		}
		if errors.Is(err, auth.ErrLastAdminProtected) {
			WriteAPIError(w, http.StatusUnprocessableEntity, "last_admin_protected", "cannot delete last admin")
			return
		}
		WriteAPIError(w, http.StatusInternalServerError, "delete_failed", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"ok":true}`))
}