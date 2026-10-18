// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

package protocol

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/angryang/ai-monitor/internal/config"
)

type stubAdapter struct{ name string }

func (s *stubAdapter) Name() string                                       { return s.name }
func (s *stubAdapter) Validate(_ *config.TaskConfig) error                { return nil }
func (s *stubAdapter) Probe(_ context.Context, _ *config.TaskConfig) (ProbeStream, error) { return nil, nil }

func TestRegistry_RegisterAndGet(t *testing.T) {
	defer Reset()
	Register("stub-a", func(d AdapterDeps) ChatProtocolAdapter { return &stubAdapter{"stub-a"} })
	SetDeps(AdapterDeps{HTTPClient: http.DefaultClient, UserAgent: "test"})

	a := MustGet("stub-a")
	if a.Name() != "stub-a" {
		t.Fatalf("Name: got %q", a.Name())
	}
}

func TestRegistry_RegisterOverwrite(t *testing.T) {
	defer Reset()
	Register("dup", func(d AdapterDeps) ChatProtocolAdapter { return &stubAdapter{"dup"} })

	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic on duplicate register")
		}
	}()
	Register("dup", func(d AdapterDeps) ChatProtocolAdapter { return &stubAdapter{"dup"} })
}

func TestRegistry_MustGetUnknown(t *testing.T) {
	defer Reset()
	SetDeps(AdapterDeps{})
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic on MustGet(unknown)")
		}
	}()
	_ = MustGet("nope")
}

func TestRegistry_List(t *testing.T) {
	defer Reset()
	Register("z", func(d AdapterDeps) ChatProtocolAdapter { return &stubAdapter{"z"} })
	Register("a", func(d AdapterDeps) ChatProtocolAdapter { return &stubAdapter{"a"} })
	Register("m", func(d AdapterDeps) ChatProtocolAdapter { return &stubAdapter{"m"} })

	got := List()
	want := []string{"a", "m", "z"}
	if len(got) != len(want) {
		t.Fatalf("List: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("List[%d]: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestRegistry_Lookup_NotFound(t *testing.T) {
	defer Reset()
	_, err := Lookup("missing")
	if err == nil {
		t.Fatalf("expected error for missing adapter")
	}
	if !errors.Is(err, err) {
		t.Logf("Lookup returned error: %v", err) // sanity: at least non-nil
	}
}

// TestRegistry_Concurrent exercises the mutex by hammering the registry
// from many goroutines; it must NOT race.
func TestRegistry_Concurrent(t *testing.T) {
	defer Reset()
	Register("ct", func(d AdapterDeps) ChatProtocolAdapter { return &stubAdapter{"ct"} })
	SetDeps(AdapterDeps{})

	const goroutines = 64
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = MustGet("ct")
				_ = List()
			}
		}()
	}
	wg.Wait()
}

func TestClassifyHTTPStatus(t *testing.T) {
	cases := []struct {
		code   int
		target error
	}{
		{200, nil},
		{301, nil},
		{401, ErrUnauthorized},
		{403, ErrUnauthorized},
		{404, ErrProtocolCfg},
		{429, ErrProtocolCfg},
		{500, ErrUpstreamDown},
		{503, ErrUpstreamDown},
	}
	for _, c := range cases {
		got := ClassifyHTTPStatus(c.code)
		if c.target == nil {
			if got != nil {
				t.Fatalf("code %d: expected nil, got %v", c.code, got)
			}
			continue
		}
		if !errors.Is(got, c.target) {
			t.Fatalf("code %d: got %v, want %v", c.code, got, c.target)
		}
	}
}
