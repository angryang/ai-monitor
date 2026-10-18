package auth

import (
	"context"
	"errors"
	"time"
)

// Sentinel errors returned by UserStore implementations. Callers match with
// errors.Is to drive 401/404/409 response codes consistently.
var (
	ErrUserNotFound       = errors.New("auth: user not found")
	ErrUserAlreadyExists  = errors.New("auth: user already exists")
	ErrUserDisabled       = errors.New("auth: user disabled")
	ErrLastAdminProtected = errors.New("auth: cannot delete or demote last admin")
	ErrSelfDeleteForbidden = errors.New("auth: cannot delete self")
)

// User is the persisted user profile. Fields mirror the §18.3 schema with
// extra runtime-only annotations documented inline.
//
// Username/DisplayName are exposed verbatim; PasswordHash MUST NEVER be
// marshalled into a JSON response. Handlers use the UserResponse helper to
// redact the field before serialising.
type User struct {
	ID          string
	Username    string
	DisplayName string
	Role        Role
	Enabled     bool
	PasswordHash string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// PublicView returns a copy of u with secret-bearing columns redacted. The
// returned struct has the same shape as User but with a different type so
// the JSON encoder emits "password_hash": "" instead of the bcrypt blob.
func (u User) PublicView() UserView {
	return UserView{
		ID:          u.ID,
		Username:    u.Username,
		DisplayName: u.DisplayName,
		Role:        string(u.Role),
		Enabled:     u.Enabled,
		CreatedAt:   u.CreatedAt,
		UpdatedAt:   u.UpdatedAt,
	}
}

// UserView is the JSON-safe shape returned to clients.
type UserView struct {
	ID          string    `json:"id"`
	Username    string    `json:"username"`
	DisplayName string    `json:"display_name"`
	Role        string    `json:"role"`
	Enabled     bool      `json:"enabled"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// CreateUserInput captures the fields a POST /admin/users body supplies.
// Passwords enter plain here and are hashed by the store before persistence
// — handlers never touch PasswordHash.
type CreateUserInput struct {
	Username    string
	DisplayName string
	Password    string
	Role        Role
	Enabled     bool
}

// UpdateUserInput is the PATCH body shape. Empty fields are treated as
// "leave unchanged"; the store fills only the supplied columns.
type UpdateUserInput struct {
	DisplayName *string
	Password    *string
	Role        *Role
	Enabled     *bool
}

// UserStore is the CRUD surface backed by SQLite (and tests' in-memory
// fake). Implementations must be safe for concurrent calls.
type UserStore interface {
	Create(ctx context.Context, in CreateUserInput) (User, error)
	GetByUsername(ctx context.Context, username string) (User, error)
	GetByID(ctx context.Context, id string) (User, error)
	List(ctx context.Context) ([]User, error)
	Update(ctx context.Context, id string, in UpdateUserInput) (User, error)
	Delete(ctx context.Context, id string) error
	CountAdmins(ctx context.Context) (int, error)
	ChangePassword(ctx context.Context, id, oldPlain, newPlain string) error
	Close() error
}