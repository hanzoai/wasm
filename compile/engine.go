// Copyright © 2026 Hanzo AI. MIT License.

package compile

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// Config is how a checker is built: which module, where the compilation cache
// lives, and the hard memory ceiling its guests get.
type Config struct {
	// Blob is the wasm module.
	Blob Blob
	// Cache is a directory wazero keeps compiled machine code in. Compiling
	// tsgo from scratch costs ~12s and reading it back ~280ms, so a host that
	// restarts without a cache pays the 12s again. Empty keeps the cache in
	// memory, which only a test wants.
	Cache string
	// Memory is the hard cap on one guest's linear memory in MiB. Empty takes
	// 256, which is what a minimal-lib tsgo check fits in. wazero enforces it
	// in pages; a guest that asks for more traps rather than growing.
	Memory uint32
}

const (
	defaultMemory = uint32(256)
	pagesPerMiB   = 16 // a wasm page is 64 KiB
)

func (c Config) memory() uint32 {
	if c.Memory == 0 {
		return defaultMemory
	}
	return c.Memory
}

// engine is one wazero runtime and one compiled module: compile once,
// instantiate many. The CompiledModule is stateless and shared; every check
// gets its own instance with its own linear memory and its own name, which is
// what lets eight run at once at 68ms each instead of one at a time.
type engine struct {
	rt    wazero.Runtime
	cache wazero.CompilationCache
	mod   wazero.CompiledModule
	seq   atomic.Uint64
	cap   uint32 // hard memory ceiling in MiB
}

func newEngine(ctx context.Context, c Config) (*engine, error) {
	src, err := c.Blob.Bytes(ctx)
	if err != nil {
		return nil, err
	}
	e := &engine{cap: c.memory()}
	rc := wazero.NewRuntimeConfig().
		WithMemoryLimitPages(e.cap * pagesPerMiB).
		WithCloseOnContextDone(true)
	if c.Cache != "" {
		e.cache, err = wazero.NewCompilationCacheWithDir(c.Cache)
		if err != nil {
			return nil, fmt.Errorf("compile: compilation cache: %w", err)
		}
		rc = rc.WithCompilationCache(e.cache)
	}
	e.rt = wazero.NewRuntimeWithConfig(ctx, rc)
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, e.rt); err != nil {
		e.Close(ctx)
		return nil, fmt.Errorf("compile: wasi: %w", err)
	}
	e.mod, err = e.rt.CompileModule(ctx, src)
	if err != nil {
		e.Close(ctx)
		return nil, fmt.Errorf("compile: compile module: %w", err)
	}
	return e, nil
}

// name returns an instance name no live instance holds. wazero refuses a
// duplicate, so concurrent checks need distinct ones.
func (e *engine) name(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, e.seq.Add(1))
}

// gomemlimit is the guest's GC ceiling for one run. It shrinks tsgo's RSS from
// 795 to 534 MiB on its own; with a minimal lib it is the difference between
// 2.35 GiB and 895 MiB across a fleet of eight. Above the hard cap it is
// refused: a GC target the linear memory cannot hold is a trap waiting for the
// second file.
func (e *engine) gomemlimit(want uint32) (string, error) {
	if want == 0 {
		want = e.cap
	}
	if want > e.cap {
		return "", fmt.Errorf("compile: %d MiB asked for, host cap is %d MiB", want, e.cap)
	}
	return fmt.Sprintf("%dMiB", want), nil
}

func (e *engine) Close(ctx context.Context) error {
	var err error
	if e.rt != nil {
		err = e.rt.Close(ctx)
	}
	if e.cache != nil {
		if cerr := e.cache.Close(ctx); err == nil {
			err = cerr
		}
	}
	return err
}
