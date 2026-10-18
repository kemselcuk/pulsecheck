package config

import "sync/atomic"

// Atomic holds the configuration the process is currently running with. The
// initial value is installed at startup; a SIGHUP reload swaps it for a freshly
// parsed one (D41). Every request handler reads the current value per request
// via Load, so a swap becomes visible everywhere atomically without
// restarting anything.
//
// A Config value is never mutated after Parse returns, which makes the
// pointer swap sufficient: readers can keep using a loaded config even while a
// newer one is installed, and the old value is simply garbage-collected once
// its last reader drops it.
//
// Atomic is safe for concurrent use; Load has no locks.
type Atomic struct {
	p atomic.Pointer[Config]
}

// NewAtomic returns a holder with cfg installed. cfg must be non-nil.
func NewAtomic(cfg *Config) *Atomic {
	a := &Atomic{}
	a.Swap(cfg)
	return a
}

// Load returns the current configuration. The result is never nil.
func (a *Atomic) Load() *Config {
	return a.p.Load()
}

// Swap installs cfg as the current configuration.
func (a *Atomic) Swap(cfg *Config) {
	a.p.Store(cfg)
}
