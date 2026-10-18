package auth

import (
	"errors"
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

// BcryptCost is the work factor used by NewBcryptHasher. Cost 12 yields
// ~250ms per hash on a 2024-era x86 server — slow enough to deter brute
// force but tolerable for a single login. Lower cost would weaken the
// offline-attack story; higher cost would push interactive logins past
// human-perceptible latency.
const BcryptCost = 12

// MinPasswordLen is the lower bound enforced at seed/change-password time.
// bcrypt silently truncates inputs > 72 bytes; we reject anything below 12
// characters to keep the user-space answerable one line of validation.
const MinPasswordLen = 12

var (
	ErrPasswordTooShort = errors.New("auth: password too short")
	ErrPasswordMismatch = errors.New("auth: password mismatch")
)

// PasswordHasher is the public surface used by Store and the HTTP layer.
type PasswordHasher interface {
	Hash(plain string) (string, error)
	Verify(hash, plain string) bool
}

// BcryptHasher wraps bcrypt with the project-standard cost factor.
type BcryptHasher struct {
	cost int
}

// NewBcryptHasher returns a hasher with the package-standard cost.
func NewBcryptHasher() *BcryptHasher { return &BcryptHasher{cost: BcryptCost} }

// Hash produces a bcrypt hash of plain. The function refuses too-short
// passwords so callers can't accidentally lower the bar on the seed path.
func (b *BcryptHasher) Hash(plain string) (string, error) {
	if len(plain) < MinPasswordLen {
		return "", fmt.Errorf("auth: %d < %d: %w", len(plain), MinPasswordLen, ErrPasswordTooShort)
	}
	h, err := bcrypt.GenerateFromPassword([]byte(plain), b.cost)
	if err != nil {
		return "", fmt.Errorf("auth: bcrypt generate: %w", err)
	}
	return string(h), nil
}

// Verify returns true when plain matches the stored hash. It uses
// bcrypt.CompareHashAndPassword under the hood, which is constant-time.
// Wrong hash, tampered hash, and wrong password all collapse to false so
// the surface doesn't leak which case tripped.
func (b *BcryptHasher) Verify(hash, plain string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}