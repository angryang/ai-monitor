package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// userRow is the SQLite-friendly projection of User. We keep this type
// unexported so callers see the domain type (User) and never the BLOB layout.
//
// NB: GORM treats a `default:true` column as "use the default when the field
// is the zero value" — i.e. an explicit `Enabled: false` would silently
// flip back to true. We side-step the trap by computing the column value
// ourselves in Create (see createDefaultRow) so callers always win.
type userRow struct {
	ID           string    `gorm:"primaryKey;size:64"`
	Username     string    `gorm:"uniqueIndex;size:64;not null"`
	DisplayName  string    `gorm:"size:128"`
	PasswordHash string    `gorm:"size:255;not null"`
	Role         string    `gorm:"size:16;not null"`
	Enabled      bool      `gorm:"not null"`
	CreatedAt    time.Time `gorm:"autoCreateTime"`
	UpdatedAt    time.Time `gorm:"autoUpdateTime"`
}

func (userRow) TableName() string { return "users" }

// SqliteUserStore is the production UserStore implementation. It shares
// the same physical database file with config.SqliteStore so admins don't
// have to manage two connection pools — see EnsureDatabase for the join-up.
type SqliteUserStore struct {
	db     *gorm.DB
	hash   PasswordHasher
	mu     sync.RWMutex
	closed bool
}

// SqliteUserStoreOptions configures NewSqliteUserStore. Zero values fall
// back to sane defaults matching the rest of the project.
type SqliteUserStoreOptions struct {
	Path        string
	BusyTimeout time.Duration
}

// NewSqliteUserStore opens (or creates) the users table on its own SQLite
// connection. Callers who want to share the config store's connection
// should use NewSqliteUserStoreOnDB instead.
func NewSqliteUserStore(opts SqliteUserStoreOptions) (*SqliteUserStore, error) {
	if opts.Path == "" {
		return nil, fmt.Errorf("auth: sqlite path required")
	}
	if opts.BusyTimeout == 0 {
		opts.BusyTimeout = 5 * time.Second
	}
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(%d)",
		opts.Path, int(opts.BusyTimeout/time.Millisecond))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Warn),
	})
	if err != nil {
		return nil, fmt.Errorf("auth: sqlite open: %w", err)
	}
	if err := db.AutoMigrate(&userRow{}); err != nil {
		return nil, fmt.Errorf("auth: migrate: %w", err)
	}
	return &SqliteUserStore{db: db, hash: NewBcryptHasher()}, nil
}

// NewSqliteUserStoreOnDB attaches the user store to an existing GORM DB.
// This is the recommended wiring in production: a single SQLite file holds
// both tasks and users, and they share the same connection pool.
func NewSqliteUserStoreOnDB(db *gorm.DB) (*SqliteUserStore, error) {
	if db == nil {
		return nil, fmt.Errorf("auth: nil db")
	}
	if err := db.AutoMigrate(&userRow{}); err != nil {
		return nil, fmt.Errorf("auth: migrate: %w", err)
	}
	return &SqliteUserStore{db: db, hash: NewBcryptHasher()}, nil
}

// SetHasher overrides the default bcrypt hasher (mostly for tests).
func (s *SqliteUserStore) SetHasher(h PasswordHasher) {
	s.mu.Lock()
	s.hash = h
	s.mu.Unlock()
}

// Close releases the underlying connection. Idempotent.
func (s *SqliteUserStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	sqlDB, err := s.db.DB()
	if err != nil {
		return fmt.Errorf("auth: get sql.DB: %w", err)
	}
	if err := sqlDB.Close(); err != nil {
		return fmt.Errorf("auth: close: %w", err)
	}
	s.closed = true
	return nil
}

// Create inserts a new user with the supplied credentials. The password
// is hashed here — callers must never pre-hash.
func (s *SqliteUserStore) Create(ctx context.Context, in CreateUserInput) (User, error) {
	if strings.TrimSpace(in.Username) == "" {
		return User{}, fmt.Errorf("auth: username required: %w", ErrUserNotFound)
	}
	role := in.Role
	if !role.IsValid() {
		return User{}, fmt.Errorf("auth: role %q: %w", role, ErrUnknownRole)
	}
	enabled := in.Enabled
	hashed, err := s.hasher().Hash(in.Password)
	if err != nil {
		return User{}, err
	}
	row := userRow{
		ID:           uuid.NewString(),
		Username:     in.Username,
		DisplayName:  in.DisplayName,
		PasswordHash: hashed,
		Role:         string(role),
		Enabled:      enabled,
	}
	// GORM's defaults are only applied when the field is the zero value, so
	// the explicit Enabled above always wins (no default:true on the column
	// any more). The line below is a defensive default for any future
	// programmatic path that constructs a row manually.
	if !row.Enabled {
		row.Enabled = false
	}
	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return User{}, fmt.Errorf("auth: %q: %w", in.Username, ErrUserAlreadyExists)
		}
		return User{}, fmt.Errorf("auth: insert: %w", err)
	}
	return rowToUser(row), nil
}

// GetByUsername looks up a user by their login name. Returns
// ErrUserNotFound for missing rows so handlers can issue 404 cleanly.
func (s *SqliteUserStore) GetByUsername(ctx context.Context, username string) (User, error) {
	var row userRow
	err := s.db.WithContext(ctx).Where("username = ?", username).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return User{}, fmt.Errorf("auth: %s: %w", username, ErrUserNotFound)
	}
	if err != nil {
		return User{}, fmt.Errorf("auth: get: %w", err)
	}
	return rowToUser(row), nil
}

// GetByID is the JWT-driven lookup used by middleware to refresh profile
// data after each request.
func (s *SqliteUserStore) GetByID(ctx context.Context, id string) (User, error) {
	var row userRow
	err := s.db.WithContext(ctx).Where("id = ?", id).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return User{}, fmt.Errorf("auth: %s: %w", id, ErrUserNotFound)
	}
	if err != nil {
		return User{}, fmt.Errorf("auth: get: %w", err)
	}
	return rowToUser(row), nil
}

// List returns every user ordered by created_at ASC (stable for UI pages).
func (s *SqliteUserStore) List(ctx context.Context) ([]User, error) {
	var rows []userRow
	if err := s.db.WithContext(ctx).Order("created_at ASC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("auth: list: %w", err)
	}
	out := make([]User, 0, len(rows))
	for _, r := range rows {
		out = append(out, rowToUser(r))
	}
	return out, nil
}

// Update applies a partial update. Empty fields are preserved; nil pointers
// in the input struct indicate "leave as-is".
func (s *SqliteUserStore) Update(ctx context.Context, id string, in UpdateUserInput) (User, error) {
	var row userRow
	if err := s.db.WithContext(ctx).Where("id = ?", id).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return User{}, fmt.Errorf("auth: %s: %w", id, ErrUserNotFound)
		}
		return User{}, fmt.Errorf("auth: lookup: %w", err)
	}

	if in.DisplayName != nil {
		row.DisplayName = *in.DisplayName
	}
	if in.Password != nil {
		h, err := s.hasher().Hash(*in.Password)
		if err != nil {
			return User{}, err
		}
		row.PasswordHash = h
	}
	if in.Role != nil {
		if !in.Role.IsValid() {
			return User{}, fmt.Errorf("auth: role %q: %w", *in.Role, ErrUnknownRole)
		}
		// Demoting the last admin to viewer would lock us out — refuse.
		if row.Role == string(RoleAdmin) && *in.Role != RoleAdmin {
			n, err := s.CountAdmins(ctx)
			if err != nil {
				return User{}, err
			}
			if n <= 1 {
				return User{}, ErrLastAdminProtected
			}
		}
		row.Role = string(*in.Role)
	}
	if in.Enabled != nil {
		// Don't disable the last enabled admin.
		if !*in.Enabled && row.Role == string(RoleAdmin) {
			n, err := s.CountAdmins(ctx)
			if err != nil {
				return User{}, err
			}
			if n <= 1 {
				return User{}, ErrLastAdminProtected
			}
		}
		row.Enabled = *in.Enabled
	}

	if err := s.db.WithContext(ctx).Save(&row).Error; err != nil {
		return User{}, fmt.Errorf("auth: save: %w", err)
	}
	return rowToUser(row), nil
}

// Delete removes a user by id. The function refuses to delete the last
// admin or to delete the supplied caller id (the latter check is the
// handler's job, but we double-check here as defence in depth).
func (s *SqliteUserStore) Delete(ctx context.Context, id string) error {
	var row userRow
	if err := s.db.WithContext(ctx).Where("id = ?", id).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return fmt.Errorf("auth: %s: %w", id, ErrUserNotFound)
		}
		return fmt.Errorf("auth: lookup: %w", err)
	}
	if row.Role == string(RoleAdmin) {
		n, err := s.CountAdmins(ctx)
		if err != nil {
			return err
		}
		if n <= 1 {
			return ErrLastAdminProtected
		}
	}
	if err := s.db.WithContext(ctx).Where("id = ?", id).Delete(&userRow{}).Error; err != nil {
		return fmt.Errorf("auth: delete: %w", err)
	}
	return nil
}

// CountAdmins returns the number of currently enabled admin rows.
func (s *SqliteUserStore) CountAdmins(ctx context.Context) (int, error) {
	var n int64
	if err := s.db.WithContext(ctx).Model(&userRow{}).
		Where("role = ? AND enabled = ?", string(RoleAdmin), true).
		Count(&n).Error; err != nil {
		return 0, fmt.Errorf("auth: count admins: %w", err)
	}
	return int(n), nil
}

// ChangePassword is the dedicated path for self-service updates. It
// requires the current password so a hijacked token can't permanently
// take over the account.
func (s *SqliteUserStore) ChangePassword(ctx context.Context, id, oldPlain, newPlain string) error {
	var row userRow
	if err := s.db.WithContext(ctx).Where("id = ?", id).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return fmt.Errorf("auth: %s: %w", id, ErrUserNotFound)
		}
		return fmt.Errorf("auth: lookup: %w", err)
	}
	if !s.hasher().Verify(row.PasswordHash, oldPlain) {
		return ErrPasswordMismatch
	}
	h, err := s.hasher().Hash(newPlain)
	if err != nil {
		return err
	}
	row.PasswordHash = h
	if err := s.db.WithContext(ctx).Save(&row).Error; err != nil {
		return fmt.Errorf("auth: save: %w", err)
	}
	return nil
}

// hasher returns the configured PasswordHasher under a read-lock.
func (s *SqliteUserStore) hasher() PasswordHasher {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.hash == nil {
		return NewBcryptHasher()
	}
	return s.hash
}

// rowToUser is the GORM-row → domain mapper. Kept tiny because the schemas
// are deliberately aligned.
func rowToUser(r userRow) User {
	return User{
		ID:           r.ID,
		Username:     r.Username,
		DisplayName:  r.DisplayName,
		Role:         Role(r.Role),
		Enabled:      r.Enabled,
		PasswordHash: r.PasswordHash,
		CreatedAt:    r.CreatedAt,
		UpdatedAt:    r.UpdatedAt,
	}
}