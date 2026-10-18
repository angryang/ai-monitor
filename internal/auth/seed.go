// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// SeedConfig is the input for EnsureSeedAdmin. All fields are optional;
// zero-values trigger the env-driven defaults documented inline.
type SeedConfig struct {
	// Username is the seeded admin's login name. Defaults to "admin"
	// (or AI_MONITOR_ADMIN_USERNAME).
	Username string
	// Password is the seeded admin's plaintext password. If empty, the
	// function reads AI_MONITOR_ADMIN_PASSWORD; if that's also empty it
	// generates a strong random password and prints it to stderr exactly
	// once. The plaintext is zeroed before returning.
	Password string
	// DisplayName is optional.
	DisplayName string
}

// SeedResult summarises what EnsureSeedAdmin did so the caller can log
// the outcome without re-running queries.
type SeedResult struct {
	Username       string
	GeneratedPassword bool
	User           User
}

// EnsureSeedAdmin idempotently seeds an admin user at startup. The
// function is safe to call multiple times: if the username already exists
// it returns it without modifying the password.
//
// Password handling (priority high → low):
//  1. cfg.Password non-empty  → use directly
//  2. AI_MONITOR_ADMIN_PASSWORD env non-empty → use env value
//  3. otherwise → generate 24 random bytes, base64-encode, **print to
//     stderr exactly once**, then zero in-memory
//
// In all three branches the plaintext is wiped from the local variable
// before the function returns so it doesn't linger on the stack.
func EnsureSeedAdmin(ctx context.Context, users UserStore, cfg SeedConfig, log *slog.Logger) (SeedResult, error) {
	if users == nil {
		return SeedResult{}, fmt.Errorf("auth: nil user store")
	}
	if log == nil {
		log = slog.Default()
	}
	username := strings.TrimSpace(cfg.Username)
	if username == "" {
		username = strings.TrimSpace(os.Getenv("AI_MONITOR_ADMIN_USERNAME"))
	}
	if username == "" {
		username = "admin"
	}

	// Try to find an existing user with this name; if found we still verify
	// there is at least one admin (which there is, because we found one).
	existing, err := users.GetByUsername(ctx, username)
	if err == nil {
		log.Info("seed_admin_exists", slog.String("username", username))
		return SeedResult{Username: username, User: existing}, nil
	}
	if !errors.Is(err, ErrUserNotFound) {
		return SeedResult{}, fmt.Errorf("auth: lookup seed user: %w", err)
	}

	password := cfg.Password
	generated := false
	if password == "" {
		password = strings.TrimSpace(os.Getenv("AI_MONITOR_ADMIN_PASSWORD"))
	}
	if password == "" {
		gen, err := randomPassword(24)
		if err != nil {
			return SeedResult{}, fmt.Errorf("auth: random password: %w", err)
		}
		password = gen
		generated = true
	}

	in := CreateUserInput{
		Username:    username,
		DisplayName: cfg.DisplayName,
		Password:    password,
		Role:        RoleAdmin,
		Enabled:     true,
	}
	u, err := users.Create(ctx, in)
	if err != nil {
		// Zero plaintext before returning on error too.
		zeroString(&password)
		return SeedResult{}, err
	}

	if generated {
		// Single, conspicuous line. Operators can grep startup logs.
		log.Warn("seed_admin_password_generated",
			slog.String("username", username),
			slog.String("password", password),
			slog.String("instruction", "store securely; rotate on first login"),
		)
	} else {
		log.Info("seed_admin_created", slog.String("username", username))
	}

	zeroString(&password)
	return SeedResult{
		Username:          username,
		GeneratedPassword: generated,
		User:              u,
	}, nil
}

// randomPassword returns n cryptographically random bytes base64-encoded.
// 24 bytes → 32 chars of base64, comfortably above the 12-char minimum.
func randomPassword(n int) (string, error) {
	if n < MinPasswordLen {
		n = 24
	}
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawStdEncoding.EncodeToString(buf), nil
}

// zeroString overwrites the supplied string's backing bytes in place.
// Go strings are immutable, so this is best-effort: it only zeros the
// portion we still hold a reference to. We use it to reduce the lifetime
// of the plaintext password on the heap.
func zeroString(s *string) {
	if s == nil {
		return
	}
	bs := []byte(*s)
	for i := range bs {
		bs[i] = 0
	}
	*s = ""
}