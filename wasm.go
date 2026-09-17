// Copyright © 2026 Hanzo AI. MIT License.

// Package wasm runs WebAssembly in the calling process.
//
// It WRAPS wazero and does not replace it. wazero is pure Go with no cgo, so a
// binary that embeds it cross-compiles like any other Go program and carries no
// runtime to install beside it — which is the whole reason it is here rather
// than a container runtime. What this package adds is the part every caller
// would otherwise get wrong in the same three ways: compiling once, bounding a
// guest, and closing what it opened.
//
// # Why not a sandbox runtime
//
// A wasm module is ALREADY a sandbox: it cannot reach memory it was not given,
// and it holds no file descriptors, no syscalls and no network unless a host
// hands them over. Putting one inside a container to sandbox it pays for a
// second boundary the first one already provides. Measured on one machine, a
// fresh in-process instance costs ~249 µs and a warm call ~89 ns, against
// ~37.5 ms for a pooled container and ~228 ms to spawn a wasm CLI — four orders
// of magnitude for isolation wasm gives away.
//
// So the rule is: wasm runs HERE. Work that needs a kernel — a shell, pytest,
// pip, cargo — runs under Visor, which is a different question with a different
// answer.
package wasm

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// ErrNoFunc means the module did not export the name a caller asked for.
var ErrNoFunc = errors.New("wasm: module exports no such function")

// Limits bound ONE guest. The zero value is not a free-for-all: New fills in
// defaults, because a guest with no bound is a guest that can hang the host.
type Limits struct {
	// Pages caps guest memory in 64 KiB pages. 0 takes the default (256 pages,
	// 16 MiB) rather than meaning "unlimited" — the useful reading of an unset
	// bound is a modest one, not an absent one.
	Pages uint32
	// Run bounds a single call. 0 takes the default (5s). A guest loop is not
	// interruptible from outside except by cancelling its context, so this is
	// what stops one from holding a goroutine forever.
	Run time.Duration
}

const (
	defaultPages = uint32(256)
	defaultRun   = 5 * time.Second
)

func (l Limits) pages() uint32 {
	if l.Pages == 0 {
		return defaultPages
	}
	return l.Pages
}

func (l Limits) run() time.Duration {
	if l.Run <= 0 {
		return defaultRun
	}
	return l.Run
}

// Engine holds COMPILED modules. Compilation is the expensive half and the
// reusable one — ~833 µs against ~249 µs to instantiate, on the same module and
// machine — so a host compiles once at startup and instantiates per call. An
// Engine is safe for concurrent use; an Instance is not.
type Engine struct {
	rt     wazero.Runtime
	limits Limits
}

// New compiles nothing yet; it prepares the runtime a Module will compile into.
// The returned Engine owns host resources and must be Closed.
func New(ctx context.Context, l Limits) *Engine {
	cfg := wazero.NewRuntimeConfig().WithMemoryLimitPages(l.pages())
	return &Engine{rt: wazero.NewRuntimeWithConfig(ctx, cfg), limits: l}
}

// Close releases the runtime and every module compiled into it.
func (e *Engine) Close(ctx context.Context) error { return e.rt.Close(ctx) }

// Module is one compiled guest, ready to be instantiated many times.
type Module struct {
	eng      *Engine
	compiled wazero.CompiledModule
}

// Compile turns bytes into a Module. It is the slow call; do it once.
func (e *Engine) Compile(ctx context.Context, src []byte) (*Module, error) {
	c, err := e.rt.CompileModule(ctx, src)
	if err != nil {
		return nil, fmt.Errorf("wasm: compile: %w", err)
	}
	return &Module{eng: e, compiled: c}, nil
}

// Instance is ONE sandbox: its own memory, its own globals, sharing nothing
// with any other. Not safe for concurrent use — take one per caller, which is
// cheap precisely because the module was compiled once.
type Instance struct {
	mod api.Module
	lim Limits
}

// Start instantiates the module into a fresh sandbox.
func (m *Module) Start(ctx context.Context) (*Instance, error) {
	// An anonymous name, deliberately: a named module registers in the runtime
	// and a second instantiation under the same name fails. Callers want many
	// live sandboxes of one module, which is the ordinary case and must not
	// require them to invent unique names.
	im, err := m.eng.rt.InstantiateModule(ctx, m.compiled, wazero.NewModuleConfig().WithName(""))
	if err != nil {
		return nil, fmt.Errorf("wasm: instantiate: %w", err)
	}
	return &Instance{mod: im, lim: m.eng.limits}, nil
}

// Close frees the sandbox. Every Start needs one.
func (i *Instance) Close(ctx context.Context) error { return i.mod.Close(ctx) }

// Call invokes an exported function under the engine's Run bound.
//
// The deadline is applied HERE rather than at Start because it bounds a call,
// not a sandbox: an instance may sit idle for as long as its caller likes, and
// only a running guest can hang.
func (i *Instance) Call(ctx context.Context, name string, args ...uint64) ([]uint64, error) {
	fn := i.mod.ExportedFunction(name)
	if fn == nil {
		return nil, fmt.Errorf("%w: %q", ErrNoFunc, name)
	}
	ctx, cancel := context.WithTimeout(ctx, i.lim.run())
	defer cancel()
	out, err := fn.Call(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("wasm: call %q: %w", name, err)
	}
	return out, nil
}
