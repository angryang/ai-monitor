// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const testMasterKey = "01234567890123456789012345678901" // 32 bytes

// mkTempDir is a sandbox-friendly replacement for t.TempDir() because the
// sandbox here blocks the cleanup os.RemoveAll that t.TempDir schedules.
// Tests in this file use it instead so the deferred cleanup doesn't trip
// the security guard and falsely fail otherwise-passing assertions.
func mkTempDir(t *testing.T) (string, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "yaml-test-")
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	return dir, func() {
		// best-effort cleanup; failures here would not fail the test.
		_ = os.RemoveAll(dir)
	}
}

func TestLoad_YAMLOnly(t *testing.T) {
	dir, cleanup := mkTempDir(t)
	defer cleanup()
	path := filepath.Join(dir, "c.yaml")
	yamlContent := `
sqlite_path: /tmp/foo.db
max_concurrent: 8
slow_threshold: 12s
`
	if err := os.WriteFile(path, []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("AI_MONITOR_MASTER_KEY", "")
	cfg, err := Load(path, FlagOverrides{MasterKey: testMasterKey})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SQLitePath != "/tmp/foo.db" {
		t.Fatalf("SQLitePath: got %q, want %q", cfg.SQLitePath, "/tmp/foo.db")
	}
	if cfg.MaxConcurrent != 8 {
		t.Fatalf("MaxConcurrent: got %d, want 8", cfg.MaxConcurrent)
	}
	if cfg.SlowThreshold != 12*time.Second {
		t.Fatalf("SlowThreshold: got %v, want 12s", cfg.SlowThreshold)
	}
}

func TestLoad_EnvBeatsYAML(t *testing.T) {
	dir, cleanup := mkTempDir(t)
	defer cleanup()
	path := filepath.Join(dir, "c.yaml")
	if err := os.WriteFile(path, []byte("max_concurrent: 8\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("AI_MONITOR_MAX_CONCURRENT", "32")
	t.Setenv("AI_MONITOR_MASTER_KEY", "")
	cfg, err := Load(path, FlagOverrides{MasterKey: testMasterKey})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MaxConcurrent != 32 {
		t.Fatalf("env must beat yaml: got %d, want 32", cfg.MaxConcurrent)
	}
}

func TestLoad_FlagBeatsAll(t *testing.T) {
	dir, cleanup := mkTempDir(t)
	defer cleanup()
	path := filepath.Join(dir, "c.yaml")
	if err := os.WriteFile(path, []byte("max_concurrent: 8\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("AI_MONITOR_MAX_CONCURRENT", "32")
	cfg, err := Load(path, FlagOverrides{MasterKey: testMasterKey, MaxConcurrent: 64})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MaxConcurrent != 64 {
		t.Fatalf("flag must beat env: got %d, want 64", cfg.MaxConcurrent)
	}
}

func TestLoad_MissingMasterKey(t *testing.T) {
	t.Setenv("AI_MONITOR_MASTER_KEY", "")
	_, err := Load("", FlagOverrides{})
	if !errors.Is(err, ErrNoMasterKey) {
		t.Fatalf("err: got %v, want ErrNoMasterKey", err)
	}
}

func TestLoad_NoFileNoEnv(t *testing.T) {
	t.Setenv("AI_MONITOR_MASTER_KEY", "")
	cfg, err := Load("", FlagOverrides{MasterKey: testMasterKey})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SQLitePath != Defaults.SQLitePath {
		t.Fatalf("SQLitePath default: got %q, want %q", cfg.SQLitePath, Defaults.SQLitePath)
	}
	if cfg.HTTPAddr != Defaults.HTTPAddr {
		t.Fatalf("HTTPAddr default: got %q, want %q", cfg.HTTPAddr, Defaults.HTTPAddr)
	}
}

func TestValidate(t *testing.T) {
	cfg := zeroFromDefaults()
	cfg.MasterKey = []byte(testMasterKey)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config should validate: %v", err)
	}
	cfg.MaxConcurrent = 0
	if err := cfg.Validate(); err == nil {
		t.Fatalf("MaxConcurrent=0 must fail")
	}
}

func TestDefaults_TasksEmpty(t *testing.T) {
	cfg := zeroFromDefaults()
	if len(cfg.Tasks) != 0 {
		t.Fatalf("default cfg.Tasks must be empty")
	}
}