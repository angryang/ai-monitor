package protocol

import (
	"fmt"
	"net/http"
	"sort"
	"sync"
)

// AdapterDeps is the dependency bundle passed from main() into each adapter
// factory at registration. We keep it small on purpose; adapters that need
// extra deps (logger, etc.) should reach them through their own singleton.
type AdapterDeps struct {
	HTTPClient *http.Client
	UserAgent  string
}

// AdapterFactory returns a freshly-constructed ChatProtocolAdapter.
type AdapterFactory func(deps AdapterDeps) ChatProtocolAdapter

var (
	mu       sync.RWMutex
	factories = make(map[string]AdapterFactory)
	stored   AdapterDeps
)

// Register installs an adapter factory under the given protocol name.
// Duplicate registration is a programming error → panic at init time.
func Register(name string, f AdapterFactory) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := factories[name]; dup {
		panic(fmt.Sprintf("protocol: adapter %q already registered", name))
	}
	factories[name] = f
}

// SetDeps freezes the runtime dependencies all subsequent MustGet calls
// receive. Set once during startup; safe to call before any MustGet.
func SetDeps(d AdapterDeps) {
	mu.Lock()
	defer mu.Unlock()
	stored = d
}

// MustGet returns an adapter instance for the protocol name, constructing
// it on demand via the registered factory. Panics if the name isn't known or
// SetDeps hasn't been called.
func MustGet(name string) ChatProtocolAdapter {
	mu.RLock()
	f, ok := factories[name]
	deps := stored
	mu.RUnlock()
	if !ok {
		panic(fmt.Sprintf("protocol: no adapter registered for %q", name))
	}
	return f(deps)
}

// Lookup behaves like MustGet but returns an error rather than panicking.
// Useful in tests and dynamic scenarios.
func Lookup(name string) (ChatProtocolAdapter, error) {
	mu.RLock()
	f, ok := factories[name]
	deps := stored
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("protocol: no adapter registered for %q", name)
	}
	return f(deps), nil
}

// List returns the registered names sorted alphabetically.
func List() []string {
	mu.RLock()
	defer mu.RUnlock()
	names := make([]string, 0, len(factories))
	for k := range factories {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// Reset wipes the registry. Tests rely on this between sub-tests to avoid
// leaking state.
func Reset() {
	mu.Lock()
	defer mu.Unlock()
	factories = make(map[string]AdapterFactory)
	stored = AdapterDeps{}
}
