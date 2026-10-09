# Memory64 support: sandbox and native AOT are different targets

Hanzo maintains two complementary Go interfaces:

| Backend | Repository | Memory64 status | Isolation |
|---|---|---|---|
| Native AOT | [hanzoai/wasm2go](https://github.com/hanzoai/wasm2go) | Memory64 addresses, WASIp1 64-bit bindings, amd64/arm64 emitters; extended conformance is gated there | **No WASM VM sandbox**. Runs as native code inside the host process |
| Sandboxed module | This repo (`hanzoai/wasm`) | **Not supported** by the pinned wazero v1.11 backend | A wazero sandbox with memory/call bounds, for wasm32 |

The `wasm` package deliberately **does not** route a Memory64 guest to native
`wasm2go`. Doing so would convert untrusted sandboxed execution into trusted
host-native execution and weaken the caller's isolation model.

## API behavior

`Engine.Compile` checks defined and imported memory declarations for the
WebAssembly Memory64 limits flag (0x04). If present, it returns
`ErrMemory64Unsupported` (usable via `errors.Is`) before invoking wazero.

```go
mod, err := engine.Compile(ctx, moduleBytes)
switch {
case errors.Is(err, wasm.ErrMemory64Unsupported):
    // Send only trusted, build-time artifacts through the native wasm2go toolchain,
    // or select a DIFFERENT sandbox engine that implements Memory64.
case err != nil:
    // Bad module or another engine failure.
default:
    // wasm32 module ready to Start.
    _ = mod
}
```

## What a true 64-bit sandbox fork requires

The API of wazero v1.11 (`api.Memory.Read`, `Write`, `Size`) takes
`uint32` offsets/lengths and does not implement the Memory64 proposal.
Implementing the proposal is **not** a cast in this wrapper. It requires a
Memory64-capable engine supporting:

1. 64-bit memory limits, growth accounting and bounds traps (including
   unsigned addition overflow and the full access width);
2. instruction decoding, validation, interpreter/compiler lowering, and
   memory.size/grow with i64 operands and results;
3. host functions with 64-bit memory pointers and separate WASI64 ABI structs;
4. safe guest memory APIs with 64-bit addresses (no silent uint32 truncation);
5. memory-growth resource budgets, cancellation and per-instance isolation;
6. differential tests against a conforming engine, and >4GiB execution on a
   runner provisioned with sufficient RAM;
7. a security review before accepting untrusted 64-bit modules.

This repository is a **wrapper**, not a fork of wazero. That invariant
remains unchanged here. True 64-bit sandbox support should be a separately
versioned backend, not hidden native execution.

## WASM32 caveat

A 64-bit Go process has native 64-bit pointers, but a
`wasm32-wasip1` guest still uses a 32-bit-indexed linear memory.
A `wasm64` guest is a different binary format with 64-bit-indexed memory.
`wasm2go` can emit native Go code from that format; this sandbox package
cannot execute it with its current backend.
