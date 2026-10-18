package protocol

// Response-body capture helper (§18.13). When an ai-monitor probe streams
// chunks back to the runner, we simultaneously capture a bounded slice of
// the raw bytes so admins can later see "what the server actually sent".
//
// The strategy is HAR-inspired: keep the first N bytes verbatim, then a
// ring-buffer of the trailing M bytes. Anything in the middle is dropped,
// and the Snapshot reports whether truncation happened so consumers can
// display a "… N bytes truncated …" marker.

import "io"

// Default response capture sizes. They balance disk usage against debug
// utility — keep them in protocol-default land so adapters don't need to
// pass them around explicitly.
const (
	DefaultRespHeadCapture = 4096 // first 4 KiB
	DefaultRespTailCapture = 4096 // last 4 KiB
	DefaultRequestCapture  = 32 * 1024 // 32 KiB hard cap on request body
)

// RespCapture buffers the bounded prefix + suffix of an io.Reader's stream.
// It implements io.Writer so the caller can wrap an HTTP body with
// io.TeeReader(body, capture) and forward only parsed chunks onward.
type RespCapture struct {
	headCap int
	tailCap int
	head    []byte
	tail    []byte // size-trimmed ring: always last tailCap bytes seen
	total   int    // total bytes Write() has observed
}

// NewRespCapture constructs a capture with the given head/tail caps. A
// non-positive cap disables that side (head = nil, tail = nil on read).
func NewRespCapture(headCap, tailCap int) *RespCapture {
	if headCap < 0 {
		headCap = 0
	}
	if tailCap < 0 {
		tailCap = 0
	}
	return &RespCapture{
		headCap: headCap,
		tailCap: tailCap,
		head:    make([]byte, 0, headCap),
	}
}

// Write implements io.Writer so the capture can be wrapped by io.TeeReader.
// It always reports the full input slice as accepted (the only error path
// would be a caller mistake that we don't need to surface here).
func (r *RespCapture) Write(p []byte) (int, error) {
	n := len(p)
	if r.headCap > 0 && len(r.head) < r.headCap {
		take := r.headCap - len(r.head)
		if take > n {
			take = n
		}
		r.head = append(r.head, p[:take]...)
	}
	if r.tailCap > 0 {
		r.tail = append(r.tail, p...)
		if len(r.tail) > r.tailCap {
			// Drop the oldest surplus.
			r.tail = r.tail[len(r.tail)-r.tailCap:]
		}
	}
	r.total += n
	return n, nil
}

// Snapshot returns the captured head, tail, and a flag indicating whether
// any bytes in the middle were dropped. Both slices may be nil when the
// corresponding cap was 0 or no bytes have been Write()'d yet.
func (r *RespCapture) Snapshot() (head, tail []byte, truncated bool) {
	if len(r.head) == 0 {
		head = nil
	} else {
		head = append([]byte(nil), r.head...) // defensive copy
	}
	if len(r.tail) == 0 {
		tail = nil
	} else {
		tail = append([]byte(nil), r.tail...)
	}
	visible := len(r.head) + len(r.tail)
	return head, tail, r.total > visible
}

// TruncateRequest applies the request-body cap (DefaultRequestCapture by
// default). Returns the (possibly truncated) body and a flag indicating
// truncation. Inputs are not modified.
func TruncateRequest(body []byte, max int) (out []byte, truncated bool) {
	if max <= 0 || len(body) <= max {
		return body, false
	}
	return body[:max], true
}

// Compile-time check.
var _ io.Writer = (*RespCapture)(nil)
