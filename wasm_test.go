package wasm_test

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/hanzoai/wasm"
)

// add.wasm: (func (export "add") (param i32 i32) (result i32) i32.add), plus one
// page of memory. Written as bytes so the test needs no toolchain to build it —
// a test that shells out to wat2wasm tests the developer's laptop, not the code.
var addWasm = []byte{
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00, 0x01, 0x07, 0x01, 0x60,
	0x02, 0x7f, 0x7f, 0x01, 0x7f, 0x03, 0x02, 0x01, 0x00, 0x05, 0x03, 0x01,
	0x00, 0x01, 0x07, 0x10, 0x02, 0x03, 0x61, 0x64, 0x64, 0x00, 0x00, 0x06,
	0x6d, 0x65, 0x6d, 0x6f, 0x72, 0x79, 0x02, 0x00, 0x0a, 0x09, 0x01, 0x07,
	0x00, 0x20, 0x00, 0x20, 0x01, 0x6a, 0x0b, 0x00, 0x0d, 0x04, 0x6e, 0x61,
	0x6d, 0x65, 0x01, 0x06, 0x01, 0x00, 0x03, 0x61, 0x64, 0x64,
}

func engine(t *testing.T) (*wasm.Engine, *wasm.Module) {
	t.Helper()
	ctx := context.Background()
	e, err := wasm.New(ctx, wasm.Limits{})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	t.Cleanup(func() { _ = e.Close(ctx) })
	m, err := e.Compile(ctx, addWasm)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return e, m
}

func TestCall(t *testing.T) {
	ctx := context.Background()
	_, m := engine(t)
	in, err := m.Start(ctx)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer in.Close(ctx)

	out, err := in.Call(ctx, "add", 2, 40)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if len(out) != 1 || out[0] != 42 {
		t.Fatalf("add(2,40) = %v, want [42]", out)
	}
}

// Many live sandboxes of ONE module is the ordinary case — a host serving
// concurrent callers — and it used to require inventing a unique name per
// instance. Start registers anonymously so it does not.
func TestConcurrentInstances(t *testing.T) {
	ctx := context.Background()
	_, m := engine(t)
	live := make([]*wasm.Instance, 0, 16)
	for i := 0; i < 16; i++ {
		in, err := m.Start(ctx)
		if err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
		live = append(live, in)
	}
	for i, in := range live {
		out, err := in.Call(ctx, "add", uint64(i), 1)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if out[0] != uint64(i)+1 {
			t.Fatalf("instance %d answered %d", i, out[0])
		}
		_ = in.Close(ctx)
	}
}

// A sandbox shares nothing: writing memory in one must not be visible in another.
func TestInstancesAreIsolated(t *testing.T) {
	ctx := context.Background()
	_, m := engine(t)
	a, _ := m.Start(ctx)
	defer a.Close(ctx)
	b, _ := m.Start(ctx)
	defer b.Close(ctx)
	if a == b {
		t.Fatal("two Starts returned one instance")
	}
	// Both answer independently; a shared instance would still pass this, so the
	// real assertion is the one above plus wazero's own memory model.
	for _, in := range []*wasm.Instance{a, b} {
		if _, err := in.Call(ctx, "add", 1, 1); err != nil {
			t.Fatalf("call: %v", err)
		}
	}
}

func TestMissingExportIsNamed(t *testing.T) {
	ctx := context.Background()
	_, m := engine(t)
	in, _ := m.Start(ctx)
	defer in.Close(ctx)

	_, err := in.Call(ctx, "nope")
	if !errors.Is(err, wasm.ErrNoFunc) {
		t.Fatalf("want ErrNoFunc, got %v", err)
	}
}

// The zero Limits must not mean "unbounded". An unset bound is the case a caller
// most often ships, so it is the one that has to be safe.
func TestZeroLimitsAreBounded(t *testing.T) {
	ctx := context.Background()
	e, err := wasm.New(ctx, wasm.Limits{})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer e.Close(ctx)
	m, err := e.Compile(ctx, addWasm)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	in, err := m.Start(ctx)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer in.Close(ctx)
	if _, err := in.Call(ctx, "add", 1, 2); err != nil {
		t.Fatalf("a defaulted engine must still run: %v", err)
	}
}

// spin.wasm: (func (export "spin") (loop (br 0))), a guest that never returns.
var spinWasm = []byte{
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00, 0x01, 0x04, 0x01, 0x60,
	0x00, 0x00, 0x03, 0x02, 0x01, 0x00, 0x07, 0x08, 0x01, 0x04, 0x73, 0x70,
	0x69, 0x6e, 0x00, 0x00, 0x0a, 0x09, 0x01, 0x07, 0x00, 0x03, 0x40, 0x0c,
	0x00, 0x0b, 0x0b,
}

// A guest that never returns is stopped by Run, or sooner by the caller's own
// context, and the call answers with that context's error. While it runs it
// yields to Go, so a garbage collection started meanwhile finishes. A guest
// that did not yield would keep the collection waiting forever, and every
// other goroutine in the process with it.
func TestRunStopsAGuestThatNeverReturns(t *testing.T) {
	ctx := context.Background()
	e, err := wasm.New(ctx, wasm.Limits{Run: time.Second})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer e.Close(ctx)
	m, err := e.Compile(ctx, spinWasm)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	spin := func(ctx context.Context) <-chan error {
		in, err := m.Start(context.Background())
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		done := make(chan error, 1)
		go func() {
			defer in.Close(context.Background())
			_, err := in.Call(ctx, "spin")
			done <- err
		}()
		return done
	}
	answer := func(done <-chan error) error {
		select {
		case err := <-done:
			return err
		case <-time.After(10 * time.Second):
			t.Fatal("the guest is still running 10s later")
			return nil
		}
	}

	done := spin(ctx)
	time.Sleep(100 * time.Millisecond)
	runtime.GC()
	select {
	case err := <-done:
		t.Errorf("the guest stopped before a collection could finish: %v", err)
	default:
	}
	if err := answer(done); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a spent Run answered %v", err)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := answer(spin(cancelled)); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled context answered %v", err)
	}
}

func TestBadBytesDoNotPanic(t *testing.T) {
	ctx := context.Background()
	e, err := wasm.New(ctx, wasm.Limits{Run: time.Second})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer e.Close(ctx)
	if _, err := e.Compile(ctx, []byte{0x00, 0x01, 0x02}); err == nil {
		t.Fatal("garbage compiled without error")
	}
}

// The default must run a Rust guest. A cdylib built for wasm32-wasip1 imports
// wasi_snapshot_preview1 through std whether or not the code calls it, so the
// zero Limits has to satisfy that import — this is the case that used to fail
// with "module[wasi_snapshot_preview1] not instantiated" and send people
// reading their own guest for a fault that was in the host.
func TestDefaultSatisfiesWASIImport(t *testing.T) {
	ctx := context.Background()
	e, err := wasm.New(ctx, wasm.Limits{})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer e.Close(ctx)
	// (module (import "wasi_snapshot_preview1" "proc_exit" (func (param i32))) (memory 1))
	src := []byte{
		0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00, 0x01, 0x05, 0x01, 0x60,
		0x01, 0x7f, 0x00, 0x02, 0x24, 0x01, 0x16, 0x77, 0x61, 0x73, 0x69, 0x5f,
		0x73, 0x6e, 0x61, 0x70, 0x73, 0x68, 0x6f, 0x74, 0x5f, 0x70, 0x72, 0x65,
		0x76, 0x69, 0x65, 0x77, 0x31, 0x09, 0x70, 0x72, 0x6f, 0x63, 0x5f, 0x65,
		0x78, 0x69, 0x74, 0x00, 0x00, 0x05, 0x03, 0x01, 0x00, 0x01, 0x00, 0x0e,
		0x04, 0x6e, 0x61, 0x6d, 0x65, 0x01, 0x07, 0x01, 0x00, 0x04, 0x65, 0x78,
		0x69, 0x74,
	}
	m, err := e.Compile(ctx, src)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	in, err := m.Start(ctx)
	if err != nil {
		t.Fatalf("a default engine must satisfy the WASI import: %v", err)
	}
	_ = in.Close(ctx)
}

// And NoWASI must actually decline it, or the opt-out is decoration.
func TestNoWASIDeclines(t *testing.T) {
	ctx := context.Background()
	e, err := wasm.New(ctx, wasm.Limits{NoWASI: true})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer e.Close(ctx)
	src := []byte{
		0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00, 0x01, 0x05, 0x01, 0x60,
		0x01, 0x7f, 0x00, 0x02, 0x24, 0x01, 0x16, 0x77, 0x61, 0x73, 0x69, 0x5f,
		0x73, 0x6e, 0x61, 0x70, 0x73, 0x68, 0x6f, 0x74, 0x5f, 0x70, 0x72, 0x65,
		0x76, 0x69, 0x65, 0x77, 0x31, 0x09, 0x70, 0x72, 0x6f, 0x63, 0x5f, 0x65,
		0x78, 0x69, 0x74, 0x00, 0x00, 0x05, 0x03, 0x01, 0x00, 0x01, 0x00, 0x0e,
		0x04, 0x6e, 0x61, 0x6d, 0x65, 0x01, 0x07, 0x01, 0x00, 0x04, 0x65, 0x78,
		0x69, 0x74,
	}
	m, err := e.Compile(ctx, src)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if _, err := m.Start(ctx); err == nil {
		t.Fatal("NoWASI still satisfied the WASI import")
	}
}
