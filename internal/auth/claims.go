package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Sentinel errors for the claims parser. They wrap jwt.ErrTokenInvalid* so
// tests and handlers can match either the generic or library-specific case.
var (
	ErrTokenInvalid = errors.New("auth: token invalid")
	ErrTokenExpired = errors.New("auth: token expired")
)

// Claims is the JWT body issued by TokenSigner. We embed jwt.RegisteredClaims
// to inherit the standard iat/exp/nbf handling and to avoid hand-rolling a
// parser.
type Claims struct {
	UserID   string `json:"uid"`
	Username string `json:"usr"`
	Role     string `json:"rol"`
	// RawToken is populated by Sign for callers that want the serialised
	// JWT back without re-running SignedString. Not serialised into the
	// token body.
	RawToken string `json:"-"`
	jwt.RegisteredClaims
}

// TokenTTL is the validity window applied to newly issued tokens. Long
// enough to cover a workday; short enough that a leaked laptop doesn't
// outlive the security rotation cycle.
const TokenTTL = 12 * time.Hour

// issuerTag is the `iss` value embedded into every claim. Mismatched issuers
// fail parse — this protects against tokens minted by a different app that
// happens to share the same secret.
const issuerTag = "ai-monitor"

// Validate enforces the structural invariants of Claims. Called by the
// signer on parse and by middleware after the library's own checks; keeping
// the validation in one place avoids drift.
func (c *Claims) Validate() error {
	if c.UserID == "" {
		return fmt.Errorf("auth: claims missing uid: %w", ErrTokenInvalid)
	}
	if c.Username == "" {
		return fmt.Errorf("auth: claims missing usr: %w", ErrTokenInvalid)
	}
	if !Role(c.Role).IsValid() {
		return fmt.Errorf("auth: claims role %q invalid: %w", c.Role, ErrTokenInvalid)
	}
	return nil
}