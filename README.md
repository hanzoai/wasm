# wasm

WebAssembly that runs **in the calling process**. A thin wrapper over
[wazero](https://github.com/tetratelabs/wazero) — not a fork of it.

```go
e, _ := wasm.New(ctx, wasm.Limits{})   // once, at startup — WASI already granted
defer e.Close(ctx)

mod, _ := e.Compile(ctx, src)          // ~833 µs — the slow half, done once
in, _ := mod.Start(ctx)                // ~249 µs — a fresh sandbox
defer in.Close(ctx)

out, _ := in.Call(ctx, "add", 2, 40)   // ~89 ns
```

## Why this is not a container

A wasm module is already a sandbox: it reaches no memory it was not given, and
holds no syscalls, no descriptors and no network unless a host hands them over.
Putting one inside a container buys a second boundary the first already
provides, and the bill is four orders of magnitude:

| path | cost | relative |
|---|---|---|
| warm call, in process | **89 ns** | 1× |
| fresh sandbox, in process | **249 µs** | 2,800× |
| pooled container | 37.5 ms | ~151× the sandbox |
| spawn a wasm CLI | 228 ms | ~917× the sandbox |

Measured on one machine, one pass, with the commands in `bench/`. Read them as
orders of magnitude rather than constants.

## What belongs here, and what does not

**Here:** pure computation on data you hand it — transforms, scoring, policy,
untrusted snippets, anything expressible as a function.

**Not here:** work that must run somebody else's binary. WASI has no process
spawn, so a guest cannot `exec` `pytest` or `cargo build`. Work that genuinely
needs a kernel runs under **Visor** (`runtimeClassName: visor`).

But "needs a kernel" is a smaller set than it first looks, and `Store` is why.

## Reaching the outside: `Store`

A guest that shells out to `git` needs `exec` and cannot run here. A guest handed
an object store does not — a repository IS objects, a read is `Get`, a write is
`Put`, and the network hop happens on the HOST side where the credential already
lives:

```go
e.Bind(ctx, s3)   // hanzoai/s3 over ZAP in the fleet, a map in a test
```

The guest imports one module, `hanzo`, with `get`/`put`/`list`. It names keys,
never hosts, and holds no credential it could leak. Nothing is spawned because
nothing needs to be — which is what makes S3-backed agentic work possible at
sandbox prices rather than container prices.

The wire is a key as `(ptr,len)` and a result written into guest-allocated
memory. No JSON crosses the boundary: bytes and two integers is the cheapest
thing that can cross, and the only shape that cannot disagree about a schema. A
guest that wants results exports `alloc(i32) i32`; one that does not gets
`ErrNoAlloc` rather than a silent truncation.

## Limits

The zero value is bounded, not unlimited: 256 pages (16 MiB) and a 5s ceiling on
one call. An unset bound is the case most callers ship, so it is the one that has
to be safe. A guest loop is not interruptible from outside except by cancelling
its context, which is what `Run` does.

## Naming

`wasm` is the package; wazero is the engine it wraps. This repo exists so that
"the Hanzo wasm runtime" has one obvious home — not to diverge from upstream. If
it ever needs engine changes, that is the moment to reconsider, and not before.

## It runs real Rust

gitoxide — pure-Rust git — compiled for `wasm32-wasip1` and parsing loose
objects inside the sandbox, measured through this package:

```
compile gitoxide module   28.3 ms   (once)
instantiate sandbox      315.7 µs
blob     -> kind 1        11.2 µs
commit   -> kind 3        28.8 µs
garbage  -> rejected       2.0 µs
```

That is the whole argument for S3-backed agentic work: a repository is objects,
`gix-object` reads them, the bytes arrive through `Store`, and nothing is
spawned. No `git` subprocess, so nothing needs `exec`, so WASI having no process
spawn stops being the objection it looks like.

Note which crates that needs: the object layer (`gix-object`, `gix-hash`) and
none of the transport ones. gitoxide is modular, and the half that wants sockets
is the half you leave out — the network hop belongs on the host, where the
credential already is.

## WASI is on by default

Because a Rust `cdylib` built for `wasm32-wasip1` imports
`wasi_snapshot_preview1` through std whether the code calls it or not. Left to
opt in, every such guest fails with `module[wasi_snapshot_preview1] not
instantiated` — an error that names the host's omission while sounding like the
guest's fault, and sends people reading their own code.

It grants nothing much: wazero starts WASI with nothing mounted, so the fd calls
exist and reach nothing. No filesystem, no network. `Limits{NoWASI: true}`
declines it for a guest that genuinely imports nothing.
