package probe

import "sync"

// slidingWindow holds recent success/failure booleans used to compute the
// success rate and consecutive-failure count. It's bounded by the configured
// window size so memory growth is O(tasks).
type slidingWindow struct {
	mu        sync.Mutex
	values    []bool
	idx       int
	full      bool
	lastN     int // tail of "latest failures" count
	lastIndex int
	cap       int
}

func newSlidingWindow(capacity int) *slidingWindow {
	if capacity <= 0 {
		capacity = 200
	}
	return &slidingWindow{cap: capacity, values: make([]bool, capacity)}
}

// Add records a new sample.
func (s *slidingWindow) Add(ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[s.idx] = ok
	s.idx = (s.idx + 1) % s.cap
	if s.idx == 0 {
		s.full = true
	}
	// Track the trailing run of failures for "consecutive failures" rule.
	if !ok {
		s.lastN = 1
	} else {
		s.lastN = 0
	}
	// Compute latest run-of-failures by scanning backwards from idx-1.
	count := 0
	cur := s.idx
	for i := 0; i < s.size(); i++ {
		cur = (cur - 1 + s.cap) % s.cap
		if s.values[cur] {
			break
		}
		count++
	}
	s.lastN = count
	s.lastIndex = s.idx
}

// size returns the number of valid samples currently stored.
func (s *slidingWindow) size() int {
	if s.full {
		return s.cap
	}
	return s.idx
}

// SuccessRate returns the fraction of ok=true samples over the window.
func (s *slidingWindow) SuccessRate() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.size()
	if n == 0 {
		return 1.0
	}
	ok := 0
	for _, v := range s.values[:n] {
		if v {
			ok++
		}
	}
	return float64(ok) / float64(n)
}

// Count returns the number of samples currently stored.
func (s *slidingWindow) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.size()
}

// ConsecutiveFailures returns the trailing run of failures (zero if the
// latest sample was success). Used for the RuleConsecFailure alert.
func (s *slidingWindow) ConsecutiveFailures() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastN
}
