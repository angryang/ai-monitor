// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func mustSecret(t *testing.T) []byte {
	t.Helper()
	b := make([]byte, MinSecretBytes)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return b
}

// ---- signer tests ----

func TestJWTSigner_SignAndParse(t *testing.T) {
	signer, err := NewJWTSigner(mustSecret(t))
	if err != nil {
		t.Fatalf("NewJWTSigner: %v", err)
	}
	c := Claims{UserID: "u-1", Username: "alice", Role: string(RoleAdmin)}
	if _, err := signer.Sign(&c); err != nil {
		t.Fatalf("sign: %v", err)
	}
	if c.RawToken == "" {
		t.Fatalf("RawToken not populated")
	}
	got, err := signer.Parse(c.RawToken)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.UserID != c.UserID || got.Username != c.Username || got.Role != c.Role {
		t.Fatalf("claims mismatch: %+v vs %+v", got, c)
	}
}

func TestJWTSigner_ExpiredRejected(t *testing.T) {
	signer, err := NewJWTSigner(mustSecret(t))
	if err != nil {
		t.Fatalf("NewJWTSigner: %v", err)
	}
	// Hand-roll an already-expired claim.
	c := Claims{
		UserID:   "u",
		Username: "u",
		Role:     string(RoleAdmin),
	}
	c.IssuedAt = jwt.NewNumericDate(time.Now().Add(-2 * TokenTTL))
	c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-TokenTTL))
	c.Issuer = issuerTag
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, &c)
	raw, err := tok.SignedString(signer.secret)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := signer.Parse(raw); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("expected ErrTokenExpired, got %v", err)
	}
}

func TestJWTSigner_AlgNoneRejected(t *testing.T) {
	signer, err := NewJWTSigner(mustSecret(t))
	if err != nil {
		t.Fatalf("NewJWTSigner: %v", err)
	}
	// Header: {"alg":"none","typ":"JWT"} → base64url (no padding)
	header := "eyJhbGciOiJub25lIiwidHlwIjoiSldUIn0"
	body := "eyJ1aWQiOiJ1IiwidXNyIjoidSIsInJvbCI6ImFkbWluIiwiaXNzIjoiYWktbW9uaXRvciIsImV4cCI6OTk5OTk5OTk5OX0"
	raw := header + "." + body + "."
	if _, err := signer.Parse(raw); err == nil {
		t.Fatalf("expected error for alg=none, got nil")
	}
}

func TestJWTSigner_TooShortSecretErr(t *testing.T) {
	if _, err := NewJWTSigner([]byte("too-short")); !errors.Is(err, ErrSecretTooShort) {
		t.Fatalf("expected ErrSecretTooShort, got %v", err)
	}
}

func TestJWTSigner_BadSignatureRejected(t *testing.T) {
	s1, _ := NewJWTSigner(mustSecret(t))
	s2, _ := NewJWTSigner(mustSecret(t))
	c := Claims{UserID: "u", Username: "u", Role: string(RoleAdmin)}
	if _, err := s1.Sign(&c); err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := s2.Parse(c.RawToken); err == nil {
		t.Fatalf("expected signature mismatch")
	}
}

// ---- bcrypt tests ----

func TestBcryptHasher_RoundTrip(t *testing.T) {
	h := NewBcryptHasher()
	hash, err := h.Hash("hunter2-supersafe!")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !h.Verify(hash, "hunter2-supersafe!") {
		t.Fatalf("verify: false for correct password")
	}
}

func TestBcryptHasher_WrongPasswordFalse(t *testing.T) {
	h := NewBcryptHasher()
	hash, _ := h.Hash("hunter2-supersafe!")
	if h.Verify(hash, "hunter2-WRONG") {
		t.Fatalf("verify: true for wrong password")
	}
}

func TestBcryptHasher_TooShortRejected(t *testing.T) {
	h := NewBcryptHasher()
	if _, err := h.Hash("short"); !errors.Is(err, ErrPasswordTooShort) {
		t.Fatalf("expected ErrPasswordTooShort, got %v", err)
	}
}

// ---- store tests ----

func newTestStore(t *testing.T) *SqliteUserStore {
	t.Helper()
	s, err := NewSqliteUserStore(SqliteUserStoreOptions{Path: ":memory:"})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestUserStore_CreateUnique(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	in := CreateUserInput{Username: "alice", Password: "supersecret-pw", Role: RoleAdmin, Enabled: true}
	if _, err := store.Create(ctx, in); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := store.Create(ctx, in); !errors.Is(err, ErrUserAlreadyExists) {
		t.Fatalf("expected ErrUserAlreadyExists, got %v", err)
	}
}

func TestUserStore_GetNotFound(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.GetByUsername(context.Background(), "ghost"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("expected ErrUserNotFound, got %v", err)
	}
}

func TestUserStore_ChangePassword(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	in := CreateUserInput{Username: "bob", Password: "old-password-12", Role: RoleViewer, Enabled: true}
	u, err := store.Create(ctx, in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.ChangePassword(ctx, u.ID, "old-password-12", "new-password-12"); err != nil {
		t.Fatalf("change: %v", err)
	}
	round, _ := store.GetByID(ctx, u.ID)
	if !store.hasher().Verify(round.PasswordHash, "new-password-12") {
		t.Fatalf("verify new password: false")
	}
	if store.hasher().Verify(round.PasswordHash, "old-password-12") {
		t.Fatalf("verify old password: true (should be false)")
	}
}

func TestUserStore_ChangePassword_BadOld(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	u, _ := store.Create(ctx, CreateUserInput{Username: "bob", Password: "old-password-12", Role: RoleViewer, Enabled: true})
	if err := store.ChangePassword(ctx, u.ID, "wrong-password", "new-password-12"); !errors.Is(err, ErrPasswordMismatch) {
		t.Fatalf("expected ErrPasswordMismatch, got %v", err)
	}
}

func TestUserStore_LastAdminProtected_Delete(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	u, _ := store.Create(ctx, CreateUserInput{Username: "root", Password: "supersecret-pw", Role: RoleAdmin, Enabled: true})
	if err := store.Delete(ctx, u.ID); !errors.Is(err, ErrLastAdminProtected) {
		t.Fatalf("expected ErrLastAdminProtected, got %v", err)
	}
}

func TestUserStore_LastAdminProtected_Demote(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	u, _ := store.Create(ctx, CreateUserInput{Username: "root", Password: "supersecret-pw", Role: RoleAdmin, Enabled: true})
	viewer := RoleViewer
	if _, err := store.Update(ctx, u.ID, UpdateUserInput{Role: &viewer}); !errors.Is(err, ErrLastAdminProtected) {
		t.Fatalf("expected ErrLastAdminProtected, got %v", err)
	}
}

func TestUserStore_LastAdminProtected_Disable(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	u, _ := store.Create(ctx, CreateUserInput{Username: "root", Password: "supersecret-pw", Role: RoleAdmin, Enabled: true})
	off := false
	if _, err := store.Update(ctx, u.ID, UpdateUserInput{Enabled: &off}); !errors.Is(err, ErrLastAdminProtected) {
		t.Fatalf("expected ErrLastAdminProtected, got %v", err)
	}
}

func TestUserStore_DeleteSecondAdminOK(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	a, _ := store.Create(ctx, CreateUserInput{Username: "root1", Password: "supersecret-pw", Role: RoleAdmin, Enabled: true})
	b, _ := store.Create(ctx, CreateUserInput{Username: "root2", Password: "supersecret-pw", Role: RoleAdmin, Enabled: true})
	if err := store.Delete(ctx, b.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.GetByID(ctx, a.ID); err != nil {
		t.Fatalf("a disappeared: %v", err)
	}
}

func TestUserStore_CountAdmins(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if n, _ := store.CountAdmins(ctx); n != 0 {
		t.Fatalf("initial: %d", n)
	}
	store.Create(ctx, CreateUserInput{Username: "a1", Password: "supersecret-pw", Role: RoleAdmin, Enabled: true})
	store.Create(ctx, CreateUserInput{Username: "a2", Password: "supersecret-pw", Role: RoleAdmin, Enabled: true})
	store.Create(ctx, CreateUserInput{Username: "v1", Password: "supersecret-pw", Role: RoleViewer, Enabled: true})
	n, _ := store.CountAdmins(ctx)
	if n != 2 {
		t.Fatalf("count: got %d, want 2", n)
	}
}

// ---- middleware tests ----

func newTestMiddleware(t *testing.T) (*Middleware, *JWTSigner, *SqliteUserStore) {
	t.Helper()
	signer, err := NewJWTSigner(mustSecret(t))
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	store := newTestStore(t)
	mw := NewMiddleware(signer, store)
	return mw, signer, store
}

func runHandler(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestMiddleware_Authenticate_MissingToken_401(t *testing.T) {
	mw, _, _ := newTestMiddleware(t)
	h := mw.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := runHandler(h, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rec.Code)
	}
}

func TestMiddleware_Authenticate_BadToken_401(t *testing.T) {
	mw, _, _ := newTestMiddleware(t)
	h := mw.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer not-a-jwt")
	rec := runHandler(h, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rec.Code)
	}
}

func TestMiddleware_Authenticate_DisabledUser_403(t *testing.T) {
	mw, signer, store := newTestMiddleware(t)
	ctx := context.Background()
	u, _ := store.Create(ctx, CreateUserInput{Username: "ghost", Password: "supersecret-pw", Role: RoleViewer, Enabled: false})
	c := Claims{UserID: u.ID, Username: u.Username, Role: string(u.Role)}
	if _, err := signer.Sign(&c); err != nil {
		t.Fatalf("sign: %v", err)
	}
	h := mw.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+c.RawToken)
	rec := runHandler(h, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403", rec.Code)
	}
}

func TestMiddleware_Authenticate_OK_200(t *testing.T) {
	mw, signer, store := newTestMiddleware(t)
	ctx := context.Background()
	u, _ := store.Create(ctx, CreateUserInput{Username: "a", Password: "supersecret-pw", Role: RoleAdmin, Enabled: true})
	c := Claims{UserID: u.ID, Username: u.Username, Role: string(u.Role)}
	if _, err := signer.Sign(&c); err != nil {
		t.Fatalf("sign: %v", err)
	}
	called := false
	h := mw.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+c.RawToken)
	rec := runHandler(h, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !called {
		t.Fatalf("handler not called")
	}
}

func TestMiddleware_RequireRole_Forbidden_403(t *testing.T) {
	mw, signer, store := newTestMiddleware(t)
	ctx := context.Background()
	u, _ := store.Create(ctx, CreateUserInput{Username: "v", Password: "supersecret-pw", Role: RoleViewer, Enabled: true})
	c := Claims{UserID: u.ID, Username: u.Username, Role: string(u.Role)}
	if _, err := signer.Sign(&c); err != nil {
		t.Fatalf("sign: %v", err)
	}
	h := mw.Authenticate(mw.RequireRole(RoleAdmin, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {})))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+c.RawToken)
	rec := runHandler(h, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403", rec.Code)
	}
}

func TestMiddleware_RequireRole_Allowed(t *testing.T) {
	mw, signer, store := newTestMiddleware(t)
	ctx := context.Background()
	u, _ := store.Create(ctx, CreateUserInput{Username: "a", Password: "supersecret-pw", Role: RoleAdmin, Enabled: true})
	c := Claims{UserID: u.ID, Username: u.Username, Role: string(u.Role)}
	if _, err := signer.Sign(&c); err != nil {
		t.Fatalf("sign: %v", err)
	}
	called := false
	h := mw.Authenticate(mw.RequireRole(RoleAdmin, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+c.RawToken)
	rec := runHandler(h, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if !called {
		t.Fatalf("handler not called")
	}
}

// ---- seed tests ----

func TestEnsureSeedAdmin_NoEnvPassword_GeneratesRandom(t *testing.T) {
	t.Setenv("AI_MONITOR_ADMIN_PASSWORD", "")
	t.Setenv("AI_MONITOR_ADMIN_USERNAME", "autoadmin")
	store := newTestStore(t)
	res, err := EnsureSeedAdmin(context.Background(), store, SeedConfig{}, nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if !res.GeneratedPassword {
		t.Fatalf("expected GeneratedPassword=true")
	}
	if res.Username != "autoadmin" {
		t.Fatalf("username: got %q", res.Username)
	}
}

func TestEnsureSeedAdmin_Idempotent(t *testing.T) {
	t.Setenv("AI_MONITOR_ADMIN_PASSWORD", "")
	store := newTestStore(t)
	res1, err := EnsureSeedAdmin(context.Background(), store, SeedConfig{Username: "admin"}, nil)
	if err != nil {
		t.Fatalf("seed 1: %v", err)
	}
	res2, err := EnsureSeedAdmin(context.Background(), store, SeedConfig{Username: "admin"}, nil)
	if err != nil {
		t.Fatalf("seed 2: %v", err)
	}
	if res1.User.ID != res2.User.ID {
		t.Fatalf("seed should be idempotent; got %s vs %s", res1.User.ID, res2.User.ID)
	}
	if res2.GeneratedPassword {
		t.Fatalf("idempotent path should NOT regenerate")
	}
}

func TestEnsureSeedAdmin_TooShortPasswordErr(t *testing.T) {
	store := newTestStore(t)
	_, err := EnsureSeedAdmin(context.Background(), store, SeedConfig{Username: "tiny", Password: "abc"}, nil)
	if !errors.Is(err, ErrPasswordTooShort) {
		t.Fatalf("expected ErrPasswordTooShort, got %v", err)
	}
}

func TestEnsureSeedAdmin_EnvPassword(t *testing.T) {
	t.Setenv("AI_MONITOR_ADMIN_PASSWORD", "from-env-pw-1234")
	store := newTestStore(t)
	res, err := EnsureSeedAdmin(context.Background(), store, SeedConfig{Username: "env"}, nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if res.GeneratedPassword {
		t.Fatalf("env should NOT trigger generation")
	}
}

// ---- role tests ----

func TestRole_Helpers(t *testing.T) {
	if !RoleAdmin.CanManageUsers() {
		t.Fatalf("admin should manage users")
	}
	if RoleViewer.CanManageUsers() {
		t.Fatalf("viewer should NOT manage users")
	}
	if !RoleAdmin.CanMutateTasks() {
		t.Fatalf("admin should mutate tasks")
	}
	if !RoleAdmin.IsValid() || !RoleViewer.IsValid() {
		t.Fatalf("valid roles")
	}
	if _, err := ParseRole("ghost"); !errors.Is(err, ErrUnknownRole) {
		t.Fatalf("expected ErrUnknownRole")
	}
}

// silence unused-import warnings if a section is commented out
var _ = http.MethodGet