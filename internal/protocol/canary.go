package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// ErrBlacklistHeader is returned when a user-supplied ExtraHeaders tries to
// set a system-controlled header.
var ErrBlacklistHeader = errors.New("protocol: header blacklisted")

// systemHeaderBlocklist is the set of headers user-supplied ExtraHeaders must
// NOT touch. The runner / proxies fully control these.
var systemHeaderBlocklist = map[string]struct{}{
	"host":           {},
	"content-length": {},
	"connection":     {},
	"transfer-encoding": {},
	"authorization":  {}, // apikey goes via dedicated field
	"x-api-key":      {}, // anthropic uses this; force via cfg.APIKey
}

// ApplyExtraHeaders parses the JSON-encoded header map and injects each entry
// into the request, skipping blacklisted entries (logged via the optional
// logger). Returns ErrBlacklistHeader when an attacker tries to override
// Authorization or system headers.
func ApplyExtraHeaders(req *http.Request, rawJSON string) error {
	if rawJSON == "" {
		return nil
	}
	headers, err := decodeHeaders(rawJSON)
	if err != nil {
		return fmt.Errorf("extra_headers: %w", err)
	}
	for k, v := range headers {
		if _, blocked := systemHeaderBlocklist[strings.ToLower(k)]; blocked {
			return fmt.Errorf("%s: %w", k, ErrBlacklistHeader)
		}
		req.Header.Set(k, v)
	}
	return nil
}

// decodeHeaders parses the JSON-encoded extras into a map.
func decodeHeaders(raw string) (map[string]string, error) {
	var out map[string]string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, err
	}
	return out, nil
}
