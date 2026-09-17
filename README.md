# wasm

WebAssembly that runs **in the calling process**. A thin wrapper over
[wazero](https://github.com/tetratelabs/wazero) — not a fork of it.

```go
e := wasm.New(ctx, wasm.Limits{})      // once, at startup
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

**Not here:** work that needs a kernel. WASI has no process spawn — no `fork`,
no `exec`, in preview 1 or the component model — so a guest cannot run `git`,
`pytest`, `pip` or `cargo`. That is not a gap to work around; it is the property
that makes the sandbox cheap. Work needing a real OS runs under **Visor**
(`runtimeClassName: visor`), which is a different question with a different
answer.

## Limits

The zero value is bounded, not unlimited: 256 pages (16 MiB) and a 5s ceiling on
one call. An unset bound is the case most callers ship, so it is the one that has
to be safe. A guest loop is not interruptible from outside except by cancelling
its context, which is what `Run` does.

## Naming

`wasm` is the package; wazero is the engine it wraps. This repo exists so that
"the Hanzo wasm runtime" has one obvious home — not to diverge from upstream. If
it ever needs engine changes, that is the moment to reconsider, and not before.
