// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

package protocol

import (
	"crypto/tls"
	"net/http"
	"testing"

	"github.com/angryang/ai-monitor/internal/config"
)

// tlsConfigOf digs the TLS config out of a client's transport so we can
// assert on InsecureSkipVerify without issuing a real handshake.
func tlsConfigOf(c *http.Client) *tls.Config {
	tr, ok := c.Transport.(*http.Transport)
	if !ok || tr == nil {
		return nil
	}
	return tr.TLSClientConfig
}

func TestClientFor_DefaultsToVerifying(t *testing.T) {
	base := &http.Client{Transport: http.DefaultTransport}
	got := ClientFor(base, false, &config.TaskConfig{})
	if got != base {
		t.Fatalf("expected the base client to be reused")
	}
	if tc := tlsConfigOf(got); tc != nil && tc.InsecureSkipVerify {
		t.Fatalf("base client must not skip verification")
	}
}

func TestClientFor_NilBaseFallsBackToDefault(t *testing.T) {
	got := ClientFor(nil, false, nil)
	if got != http.DefaultClient {
		t.Fatalf("expected http.DefaultClient fallback, got %v", got)
	}
}

func TestClientFor_TaskLevelOptIn(t *testing.T) {
	base := &http.Client{Transport: http.DefaultTransport}
	got := ClientFor(base, false, &config.TaskConfig{InsecureSkipVerify: true})
	tc := tlsConfigOf(got)
	if tc == nil {
		t.Fatalf("expected a TLS config on the insecure client")
	}
	if !tc.InsecureSkipVerify {
		t.Fatalf("expected InsecureSkipVerify=true")
	}
	// The base client must not be mutated — it is shared process-wide.
	if bt := tlsConfigOf(base); bt != nil && bt.InsecureSkipVerify {
		t.Fatalf("base client transport was mutated")
	}
}

func TestClientFor_GlobalSwitchAppliesToEveryTask(t *testing.T) {
	base := &http.Client{Transport: http.DefaultTransport}
	// Task says "verify", but the instance-wide switch says otherwise.
	got := ClientFor(base, true, &config.TaskConfig{InsecureSkipVerify: false})
	tc := tlsConfigOf(got)
	if tc == nil || !tc.InsecureSkipVerify {
		t.Fatalf("expected the global switch to force insecure mode")
	}
}

func TestInsecureHTTPClient_IsShared(t *testing.T) {
	a := InsecureHTTPClient()
	b := InsecureHTTPClient()
	if a != b {
		t.Fatalf("expected a shared client instance")
	}
	tc := tlsConfigOf(a)
	if tc == nil || !tc.InsecureSkipVerify {
		t.Fatalf("expected InsecureSkipVerify=true")
	}
}
