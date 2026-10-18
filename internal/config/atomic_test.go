package config

import (
	"sync"
	"testing"
)

// TestAtomic covers the swap/load semantics of the runtime holder (D41).
func TestAtomic(t *testing.T) {
	first := &Config{Server: ServerConfig{Addr: ":1111"}}
	second := &Config{Server: ServerConfig{Addr: ":2222"}}

	holder := NewAtomic(first)
	if got := holder.Load(); got != first {
		t.Fatalf("Load() = %p, want the initial config %p", got, first)
	}
	holder.Swap(second)
	if got := holder.Load(); got != second {
		t.Fatalf("Load() after Swap = %p, want %p", got, second)
	}
}

// TestAtomicConcurrentSwapLoad exercises the holder under the race detector:
// readers must never observe a nil or torn value while configurations swap.
func TestAtomicConcurrentSwapLoad(t *testing.T) {
	cfgs := make([]*Config, 8)
	for i := range cfgs {
		cfgs[i] = &Config{Server: ServerConfig{Addr: ":8080"}}
	}
	holder := NewAtomic(cfgs[0])

	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				holder.Swap(cfgs[(w+i)%len(cfgs)])
			}
		}(w)
	}
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				if cfg := holder.Load(); cfg == nil {
					t.Error("Load returned nil during concurrent swaps")
					return
				}
			}
		}()
	}
	wg.Wait()
}
