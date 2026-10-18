// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// MinSecretBytes is the lower bound enforced by NewJWTSigner. Anything
// shorter is rejected at startup with ErrSecretTooShort; the loader in main
// converts that error into a hard exit. 32 bytes matches the HMAC-SHA256
// key-recommendation in RFC 7518 §3.2 and keeps parity with our AES master
// key length.
const MinSecretBytes = 32

var (
	ErrSecretTooShort = errors.New("auth: jwt secret must be >= 32 bytes")
	ErrTokenSignature = errors.New("auth: token signature mismatch")
	ErrAlgNotAllowed  = errors.New("auth: token alg not allowed")
)

// TokenSigner is the public surface used by the HTTP handlers and the seed
// routine. Tests rely on the interface so they can swap in a fake.
type TokenSigner interface {
	Sign(c *Claims) (string, error)
	Parse(raw string) (*Claims, error)
}

// JWTSigner signs tokens with HS256. Only this algorithm is supported — the
// "alg=none" attack class and HS/RS confusion attacks motivated us to pin
// the library to a single, safe choice.
type JWTSigner struct {
	secret []byte
	now    func() time.Time // injectable clock for tests
}

// NewJWTSigner validates secret length and returns a signer. The secret is
// not copied; callers MUST keep it alive for the signer's lifetime (main()
// holds a single global secret).
func NewJWTSigner(secret []byte) (*JWTSigner, error) {
	if len(secret) < MinSecretBytes {
		return nil, fmt.Errorf("got %d bytes: %w", len(secret), ErrSecretTooShort)
	}
	cp := make([]byte, len(secret))
	copy(cp, secret)
	return &JWTSigner{secret: cp, now: time.Now}, nil
}

// Sign mints a new JWT. IssuedAt/ExpiresAt/Issuer are populated automatically
// so callers only set the role + identity claims. Returns the raw token and
// also stores it on c.RawToken so callers can pick whichever shape they want.
func (s *JWTSigner) Sign(c *Claims) (string, error) {
	if s == nil {
		return "", ErrSecretTooShort
	}
	if c.IssuedAt == nil {
		c.IssuedAt = jwt.NewNumericDate(s.now())
	}
	if c.ExpiresAt == nil {
		c.ExpiresAt = jwt.NewNumericDate(s.now().Add(TokenTTL))
	}
	c.Issuer = issuerTag
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, c)
	raw, err := tok.SignedString(s.secret)
	if err != nil {
		return "", fmt.Errorf("auth: sign: %w", err)
	}
	c.RawToken = raw
	return raw, nil
}

// SignAndReturn is a convenience alias kept for readability in callers
// that prefer to spell the return value out.
func (s *JWTSigner) SignAndReturn(c *Claims) (string, error) { return s.Sign(c) }

// Parse validates the signature, expiry, and issuer, then returns the typed
// claims. Any failure surfaces as one of the package sentinel errors so
// middleware can pick the right HTTP code.
func (s *JWTSigner) Parse(raw string) (*Claims, error) {
	if s == nil || len(s.secret) == 0 {
		return nil, ErrSecretTooShort
	}
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(issuerTag),
		jwt.WithExpirationRequired(),
	)
	c := &Claims{}
	tok, err := parser.ParseWithClaims(raw, c, func(t *jwt.Token) (any, error) {
		// extra defensive check: even though WithValidMethods guarantees the
		// alg, belt-and-braces verification prevents future regressions if
		// the parser options are tweaked.
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, ErrAlgNotAllowed
		}
		return s.secret, nil
	})
	if err != nil {
		switch {
		case errors.Is(err, jwt.ErrTokenExpired):
			return nil, fmt.Errorf("auth: %w: %v", ErrTokenExpired, err)
		case errors.Is(err, jwt.ErrTokenSignatureInvalid):
			return nil, fmt.Errorf("auth: %w: %v", ErrTokenSignature, err)
		default:
			return nil, fmt.Errorf("auth: %w: %v", ErrTokenInvalid, err)
		}
	}
	if !tok.Valid {
		return nil, ErrTokenInvalid
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	// constant-time compare on signature never actually happens here because
	// the library already validated it; subtle.ConstantTimeCompare on a zero
	// length slice is harmless and keeps `go vet -race` from complaining
	// about unused imports if the structure changes later.
	_ = subtle.ConstantTimeCompare
	return c, nil
}

// RandomSecret returns a cryptographically random secret of n bytes encoded
// as standard base64. Used by EnsureSeedAdmin as a fallback when the env
// didn't provide AI_MONITOR_JWT_SECRET — that path is intentionally a hard
// startup error, but RandomSecret is exported for tests.
func RandomSecret(n int) (string, error) {
	if n < MinSecretBytes {
		n = MinSecretBytes
	}
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("auth: random: %w", err)
	}
	return base64.StdEncoding.EncodeToString(buf), nil
}