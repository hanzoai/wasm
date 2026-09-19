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

## It runs real compilers: `compile`

`wasm` runs a guest. `compile` is the host for the case where the guest is a
compiler — one registry, two roles, one diagnostic shape:

```go
c, _ := compile.NewTSGo(ctx, compile.Config{
    Blob:  compile.Blob{Path: "tsgo.wasm", Sum: "b17624…"},
    Cache: "/var/cache/hanzo/wazero",
})
compile.Register(c)

diags, _ := c.Check(ctx, compile.Project{
    Root:  "/project",
    Entry: []string{"entry.ts"},
    Files: compile.Map{"entry.ts": src, "math.ts": more},
}, compile.Options{})
// entry.ts:2:14 error TS2322 Type 'number' is not assignable to type 'string'.
```

A checker is registered, not endpointed. Supporting Rust is a row rather than a
route, and `Checkers()` is how an agent learns what can be checked here instead
of hardcoding a list.

`Checker` and `Bundler` are separate interfaces because most checkers do not
bundle. esbuild bundles `const wrong: string = add(1,2)` and exits 0, so it
registers as a `Bundler` and only that — asking for `CheckerNamed("esbuild")`
gets nothing, which is the registry refusing to claim source was verified when
it was not.

### A project is files, not a path

`Project.Files` is a host callback: `ReadFile`, `FileExists`, `DirExists`,
`Entries`, `Realpath`. There is no disk path in it. The repository lives in s3,
the host answers reads out of its own cache, and the guest reaches exactly what
the host answers for and nothing else. `Map` is the in-memory implementation a
test uses and the shape a warm cache takes.

Every string that becomes one of those calls is cleaned first and refused if it
leaves the root — a guest's import, a caller's entry, the host's own `Realpath`
answer. `import "/project/../outside"` is not a file: it is a name the project
does not contain, and the guest is told so at the line that asked for it. A path
in an esbuild `on-load` request is no different — only a name some `on-resolve`
already answered for is read, because a field in a request is not authority. So
the obvious host, `os.ReadFile(filepath.Join(base, name))`, is a safe one.

esbuild needs no filesystem at all: its plugin protocol carries every resolve
and load, so the module runs with **zero preopened directories**. tsgo issues
ordinary WASI reads — porting a compiler off a filesystem would mean forking it
— so its mount is an `fs.FS` backed by the same callbacks. Either way the answer
comes from a function, and a project with no `tsconfig.json` gets one that
exists only in the guest's view of the tree.

### One diagnostic shape

`{file, line, column, severity, code, message, checker}`, lines and columns
1-based because every compiler in the set already reports that way. A column
counts UTF-16 code units, which is what a compiler counts; esbuild counts bytes
from 0, so on `import { nope as ééé } from "./gone";` it puts the opening quote
at 32 where tsgo puts it at 29. Both say 29 here, and that translation happens
once rather than in every caller.

A diagnostic about no position in a file has line and column 0 — the only thing 0
means — and still names its file when the message does: tsc reports `File
'/project/ghost.ts' not found.` with no location at all, and an agent routes by
file. An agent that reads a `TS2322` reads an `E0308` with no new code.

### What it costs

tsgo is a command module: `_start` runs `main()` and exits, so every check
re-parses the standard library, and that parse is the bill. Measured on evo
(x86_64, 32 cores, 2026-09-17) over a two-file project:

| | wall | memory |
|---|---|---|
| default libs, one check | 1.7 s | 795 MB |
| `lib: ["es2022"]`, `types: []` | 273 ms | 430 MB |
| eight at once, `GOMEMLIMIT=256MiB` | 541 ms | 895 MB total |

So es2022 with no ambient types is the default here, `GOMEMLIMIT` is set per
check, and one `CompiledModule` is instantiated many times — eight concurrent
checks at 68 ms each. Compiling the module itself costs ~12 s, which is why
`Config.Cache` is a directory: reading 49 MB of machine code back is ~300 ms.
`go test -bench .` prints cold, warm and eight-at-once for the host it runs on.

Above that fleet size the numbers turn: sixteen instances of the default-lib
module cost 5.0 s and 4.07 GB. ~290 MB per instance is the budget, and the
structural fix is a reactor — a live instance holding a parsed library that
answers per-edit checks — not more instances.

### The modules are not in this repository

tsgo is 49 MB and esbuild 20 MB. `Blob` names one by path or URL **and** its
sha256; a blob with no digest is refused rather than trusted. The tests want
`TSGO_WASM` and `ESBUILD_WASM` pointing at local builds, with `TSGO_SHA256` and
`ESBUILD_SHA256` to pin them. Without those they **fail** and name the one that
is missing. They do not skip: a run of this package that ran neither compiler has
verified nothing, and a skip prints `ok` for it.
