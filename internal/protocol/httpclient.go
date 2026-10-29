// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

package protocol

import (
	"crypto/tls"
	"net/http"
	"sync"

	"github.com/angryang/ai-monitor/internal/config"
)

// insecureClient caches the HTTP client used by probes that opt out of
// upstream TLS certificate verification.
//
// It is deliberately package-level rather than per-adapter: adapters are
// rebuilt on every probe (Lookup constructs a fresh one), so a per-adapter
// transport would leak a connection pool per probe. One shared client
// keeps connection reuse intact.
var (
	insecureOnce   sync.Once
	insecureClient *http.Client
)

// InsecureHTTPClient returns the shared client that does not verify upstream
// TLS certificates.
//
// Use it only when the endpoint presents a self-signed certificate or one
// signed by a private CA the host doesn't trust — internal model servers,
// in-cluster gateways. Skipping verification removes MITM protection, so
// keep it scoped to the tasks that actually need it.
//
// The client inherits the stdlib default transport (proxy support, dial
// timeouts, HTTP/2 negotiation) and only relaxes certificate checking.
func InsecureHTTPClient() *http.Client {
	insecureOnce.Do(func() {
		tr, ok := http.DefaultTransport.(*http.Transport)
		if !ok {
			// DefaultTransport is always an *http.Transport in the stdlib;
			// this branch only guards against a future change.
			tr = &http.Transport{}
		} else {
			tr = tr.Clone()
		}
		if tr.TLSClientConfig == nil {
			tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		} else {
			tr.TLSClientConfig = tr.TLSClientConfig.Clone()
		}
		//nolint:gosec // G402: opt-in per task, see InsecureHTTPClient docs.
		tr.TLSClientConfig.InsecureSkipVerify = true
		insecureClient = &http.Client{Transport: tr}
	})
	return insecureClient
}

// ClientFor picks the HTTP client a probe should use.
//
//   - base is the shared client handed to the adapter via AdapterDeps.
//     A nil base falls back to http.DefaultClient.
//   - globalInsecure is the instance-wide switch (AdapterDeps).
//   - cfg carries the per-task override (TaskConfig.InsecureSkipVerify).
//
// The two switches are OR'ed: a task opts out of verification either
// individually or because the operator turned the instance-wide switch on.
// Turning the instance-wide switch on is an explicit "this deployment does
// not verify certificates" decision, so it deliberately cannot be undone
// per task.
func ClientFor(base *http.Client, globalInsecure bool, cfg *config.TaskConfig) *http.Client {
	if base == nil {
		base = http.DefaultClient
	}
	if globalInsecure || (cfg != nil && cfg.InsecureSkipVerify) {
		return InsecureHTTPClient()
	}
	return base
}
