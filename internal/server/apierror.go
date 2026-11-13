// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

// Package server: shared API error envelope and helpers.
package server

import (
	"encoding/json"
	"net/http"
)

// APIError is the canonical JSON error envelope returned by every handler.
// Tests assert this exact shape via the *Auth_* and *Admin* tests.
type APIError struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
	// Details carries structured context about the failure — the verdict
	// of a provider connectivity check, for instance. It is omitted when
	// there is nothing to add, so the common shape stays exactly as it
	// was.
	Details any `json:"details,omitempty"`
}

// WriteAPIError emits the APIError as a JSON object under the "error" key
// of the response body. The function takes care of writing both headers
// and body so handlers can chain one-liners.
func WriteAPIError(w http.ResponseWriter, status int, code, message string) {
	WriteAPIErrorDetails(w, status, code, message, nil)
}

// WriteAPIErrorDetails is WriteAPIError with a structured payload attached
// to the error object. Callers use it when the client needs more than a
// message to act on — a failed connectivity check, for example, carries
// the upstream's verdict so the UI can explain what to fix.
func WriteAPIErrorDetails(w http.ResponseWriter, status int, code, message string, details any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": APIError{Status: status, Code: code, Message: message, Details: details},
	})
}

// writeJSON encodes v as a JSON object with the appropriate header.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// readJSON parses the request body into v. Returns a sentinel API error on
// failure so handlers can short-circuit.
func readJSON(r *http.Request, v any) *APIError {
	if r.Body == nil {
		return &APIError{Status: http.StatusBadRequest, Code: "empty_body", Message: "request body empty"}
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return &APIError{Status: http.StatusBadRequest, Code: "bad_json", Message: err.Error()}
	}
	return nil
}
