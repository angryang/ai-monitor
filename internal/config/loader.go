// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Defaults matches design.md §9 default parameters. Each field can be
// overridden by config.yaml, then by an env variable, then by a flag.
var Defaults = struct {
	HTTPAddr             string
	SQLitePath           string
	MaxConcurrent        int
	Jitter               time.Duration
	SlowThreshold        time.Duration
	ConsecFailureN       int
	SuccessRateWindow    int
	SuccessRateThreshold float64
	DedupTTL             time.Duration
	DefaultTimeout       time.Duration
	BucketsDefault       []float64
	BucketsTTFT          []float64
	BucketsITL           []float64
	BucketsDuration      []float64
	BucketsLag           []float64
}{
	HTTPAddr:             ":9090",
	SQLitePath:           "ai-monitor.db",
	MaxConcurrent:        16,
	Jitter:               250 * time.Millisecond,
	SlowThreshold:        10 * time.Second,
	ConsecFailureN:       3,
	SuccessRateWindow:    200,
	SuccessRateThreshold: 0.8,
	DedupTTL:             5 * time.Minute,
	DefaultTimeout:       30 * time.Second,
	BucketsDefault:       []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30},
	BucketsTTFT:          []float64{.01, .025, .05, .1, .2, .35, .5, .75, 1, 2, 5},
	BucketsITL:           []float64{.005, .01, .025, .05, .1, .25, .5},
	BucketsDuration:      []float64{.1, .25, .5, 1, 2.5, 5, 10, 20, 30, 60, 120},
	BucketsLag:           []float64{.01, .05, .1, .5, 1, 5, 30, 60},
}

// Config is the runtime configuration loaded at startup.
type Config struct {
	// MasterKey is required for AES-GCM. It is read from
	// AI_MONITOR_MASTER_KEY but never persisted. The loader returns ErrNoMasterKey
	// if it stays empty.
	MasterKey []byte
	// SQLitePath where tasks and audit live.
	SQLitePath string `yaml:"sqlite_path"`
	// HTTPAddr for the /metrics + /healthz surface. ":9090" default.
	HTTPAddr string `yaml:"http_addr"`
	// Scheduling
	MaxConcurrent int           `yaml:"max_concurrent"`
	Jitter        time.Duration `yaml:"jitter"`
	// Alerting
	SlowThreshold        time.Duration `yaml:"slow_threshold"`
	ConsecFailureN       int           `yaml:"consec_failure_n"`
	SuccessRateWindow    int           `yaml:"success_rate_window"`
	SuccessRateThreshold float64       `yaml:"success_rate_threshold"`
	DedupTTL             time.Duration `yaml:"dedup_ttl"`
	// Histogram buckets (used by metrics.PromSink).
	BucketsDefault  []float64 `yaml:"buckets_default"`
	BucketsTTFT     []float64 `yaml:"buckets_ttft"`
	BucketsITL      []float64 `yaml:"buckets_itl"`
	BucketsDuration []float64 `yaml:"buckets_duration"`
	BucketsLag      []float64 `yaml:"buckets_lag"`
	// Tasks lists tasks initially seeded into SQLite. Optional.
	Tasks []TaskConfig `yaml:"tasks"`
}

// Sentinel errors returned by Load.
var (
	ErrNoMasterKey = errors.New("config: master key required")
)

// Load merges YAML < env (AI_MONITOR_*) < flag overrides. The flag layer is
// fed via the optional FlagOverrides struct, e.g. command-line args parsed by
// cobra/flag. If a flag is non-nil it's applied last so it always wins.
//
// The function deliberately keeps precedence simple and side-effect-free;
// secrets (API key per task) are *not* loaded here — they belong to the
// ConfigStore layer because they're encrypted in SQLite.
func Load(path string, flags FlagOverrides) (*Config, error) {
	cfg := zeroFromDefaults()

	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read yaml: %w", err)
		}
		if len(raw) > 0 {
			if err := yaml.Unmarshal(raw, cfg); err != nil {
				return nil, fmt.Errorf("parse yaml: %w", err)
			}
		}
	}

	applyEnv(cfg)

	applyFlagOverrides(cfg, flags)

	if len(cfg.MasterKey) != 32 {
		return nil, fmt.Errorf("master key must be 32 bytes (got %d): %w", len(cfg.MasterKey), ErrNoMasterKey)
	}
	return cfg, nil
}

// FlagOverrides holds optional command-line overrides. A nil/zero-valued field
// means "no override" — the loader leaves the previous layer's value alone.
type FlagOverrides struct {
	MasterKey        string
	SQLitePath       string
	HTTPAddr         string
	MaxConcurrent    int
	Jitter           time.Duration
	SlowThreshold    time.Duration
	ConsecFailureN   int
	SuccessRateW     int
	SuccessRateThr   float64
	DedupTTL         time.Duration
}

// zeroFromDefaults clones the package defaults into a fresh Config.
func zeroFromDefaults() *Config {
	c := &Config{
		SQLitePath:           Defaults.SQLitePath,
		HTTPAddr:             Defaults.HTTPAddr,
		MaxConcurrent:        Defaults.MaxConcurrent,
		Jitter:               Defaults.Jitter,
		SlowThreshold:        Defaults.SlowThreshold,
		ConsecFailureN:       Defaults.ConsecFailureN,
		SuccessRateWindow:    Defaults.SuccessRateWindow,
		SuccessRateThreshold: Defaults.SuccessRateThreshold,
		DedupTTL:             Defaults.DedupTTL,
		BucketsDefault:       append([]float64(nil), Defaults.BucketsDefault...),
		BucketsTTFT:          append([]float64(nil), Defaults.BucketsTTFT...),
		BucketsITL:           append([]float64(nil), Defaults.BucketsITL...),
		BucketsDuration:      append([]float64(nil), Defaults.BucketsDuration...),
		BucketsLag:           append([]float64(nil), Defaults.BucketsLag...),
	}
	return c
}

func applyEnv(c *Config) {
	if v := os.Getenv("AI_MONITOR_MASTER_KEY"); v != "" {
		c.MasterKey = []byte(v)
	}
	if v := os.Getenv("AI_MONITOR_SQLITE_PATH"); v != "" {
		c.SQLitePath = v
	}
	if v := os.Getenv("AI_MONITOR_HTTP_ADDR"); v != "" {
		c.HTTPAddr = v
	}
	if v := os.Getenv("AI_MONITOR_MAX_CONCURRENT"); v != "" {
		if n, err := atoi(v); err == nil {
			c.MaxConcurrent = n
		}
	}
	if v := os.Getenv("AI_MONITOR_JITTER"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.Jitter = d
		}
	}
	if v := os.Getenv("AI_MONITOR_SLOW_THRESHOLD"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.SlowThreshold = d
		}
	}
}

func applyFlagOverrides(c *Config, f FlagOverrides) {
	if f.MasterKey != "" {
		c.MasterKey = []byte(f.MasterKey)
	}
	if f.SQLitePath != "" {
		c.SQLitePath = f.SQLitePath
	}
	if f.HTTPAddr != "" {
		c.HTTPAddr = f.HTTPAddr
	}
	if f.MaxConcurrent > 0 {
		c.MaxConcurrent = f.MaxConcurrent
	}
	if f.Jitter > 0 {
		c.Jitter = f.Jitter
	}
	if f.SlowThreshold > 0 {
		c.SlowThreshold = f.SlowThreshold
	}
	if f.ConsecFailureN > 0 {
		c.ConsecFailureN = f.ConsecFailureN
	}
	if f.SuccessRateW > 0 {
		c.SuccessRateWindow = f.SuccessRateW
	}
	if f.SuccessRateThr > 0 {
		c.SuccessRateThreshold = f.SuccessRateThr
	}
	if f.DedupTTL > 0 {
		c.DedupTTL = f.DedupTTL
	}
}

func atoi(s string) (int, error) {
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return 0, err
	}
	return n, nil
}

// MarshalYAML is exposed so tests/callers can render a Config back to YAML.
// We keep a small wrapper rather than expose the struct tag directly so callers
// don't have to import gopkg.in/yaml.v3 just to render config.
func (c *Config) MarshalYAML() ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(c); err != nil {
		return nil, fmt.Errorf("marshal yaml: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("close yaml encoder: %w", err)
	}
	return buf.Bytes(), nil
}

// Validate checks basic runtime invariants (positive durations, ranges, etc).
// It is intentionally loose — deep validation of protocol sections happens
// in ValidateProtocolSegments.
func (c *Config) Validate() error {
	if c.MaxConcurrent <= 0 {
		return fmt.Errorf("config: max_concurrent must be > 0")
	}
	if c.Jitter < 0 {
		return fmt.Errorf("config: jitter must be >= 0")
	}
	if c.ConsecFailureN <= 0 {
		return fmt.Errorf("config: consec_failure_n must be > 0")
	}
	if c.SuccessRateThreshold <= 0 || c.SuccessRateThreshold > 1 {
		return fmt.Errorf("config: success_rate_threshold must be in (0,1]")
	}
	if c.SuccessRateWindow <= 0 {
		return fmt.Errorf("config: success_rate_window must be > 0")
	}
	if c.DedupTTL <= 0 {
		return fmt.Errorf("config: dedup_ttl must be > 0")
	}
	if !strings.HasPrefix(c.HTTPAddr, ":") && !strings.Contains(c.HTTPAddr, ":") {
		return fmt.Errorf("config: http_addr must be a host:port string")
	}
	return nil
}
