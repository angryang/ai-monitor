// Package server: optional auth-related fields for the Server's Options
// struct. Keeping them in a separate file prevents http.go from growing
// any further while still letting callers wire everything through the
// existing Options struct.
package server

import (
	"github.com/angryang/ai-monitor/internal/auth"
	"github.com/angryang/ai-monitor/internal/config"
)

// HandlersConfig bundles the optional dependencies auth handlers need.
// Created from Options in New and consumed by handlers_test.go.
type HandlersConfig struct {
	Auth      *auth.Middleware
	UserStore auth.UserStore
	Store     config.ConfigStore
	Cipher    config.Cipher
}

// ToHandlersConfig lifts the auth pieces out of Options. Returns nil when
// auth wasn't wired so the caller can fall back to no-auth handlers.
func (o *Options) ToHandlersConfig() *HandlersConfig {
	if o == nil || o.Auth == nil {
		return nil
	}
	return &HandlersConfig{
		Auth:      o.Auth,
		UserStore: o.UserStore,
		Store:     o.Store,
		Cipher:    o.Cipher,
	}
}