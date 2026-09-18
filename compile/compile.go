// Copyright © 2026 Hanzo AI. MIT License.

// Package compile answers two questions about source: is it correct, and can it
// be built. Both are answered by code we control, deterministically, with no
// ambient authority — a wasm module, files served by the host, no filesystem,
// no network, no credential in the guest.
//
// # The registry is the design
//
// A checker is registered, not endpointed. Supporting another language is a row
// rather than a route, so the op set never grows with the language count, and
// "what can typecheck here" is a read of the same registry that runs it.
//
// Checking and bundling are separate interfaces because most checkers do not
// bundle. A type that does both registers as both; nothing forces a stub
// method, and nothing can quietly claim a capability it does not have —
// esbuild bundles `const wrong: string = add(1,2)` and exits 0, so it is a
// Bundler and not a Checker, and the registry is what keeps that honest.
//
// # Work that does not belong here
//
// Build and test run the repository's own toolchain: arbitrary native code that
// needs a kernel. That is the sandbox lane (HIP-1146), which answers with a job
// id and streams logs. This package answers in milliseconds or refuses.
package compile

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// Files is the project's bytes, answered by the host. Every name is relative to
// the project root, slash-separated, with no leading slash; "." is the root
// itself.
//
// There is no disk path in this interface on purpose. The repository lives in
// s3 and the host answers reads from its own cache, so a checker cannot reach
// anything the host did not hand it, and the same project serves a laptop, a
// test and a fleet without changing shape.
type Files interface {
	// ReadFile returns one file's bytes, or an error naming what was missing.
	ReadFile(name string) ([]byte, error)
	// FileExists reports whether name is a readable file.
	FileExists(name string) bool
	// DirExists reports whether name is a directory.
	DirExists(name string) bool
	// Entries lists the bare names directly under a directory, unordered.
	Entries(name string) ([]string, error)
	// Realpath resolves name to the name the host will answer reads under. A
	// host with no aliases returns name unchanged.
	Realpath(name string) (string, error)
}

// Project is a set of files and the entries a bundler starts from.
type Project struct {
	// Root is the name the project is mounted under inside the guest. It is a
	// label, not a location: nothing on the host's disk answers to it.
	Root string
	// Entry names the files a bundler starts from, project-relative. A checker
	// that reads a manifest ignores it.
	Entry []string
	// Files serves the bytes.
	Files Files
}

const defaultRoot = "/project"

func (p Project) root() string {
	if p.Root == "" {
		return defaultRoot
	}
	return p.Root
}

// Diagnostic is what every checker answers in. One shape, or the
// generalization is fake: an agent that can read a tsgo error reads a clippy
// error with no new code.
type Diagnostic struct {
	File     string `json:"file"`
	Line     int    `json:"line"`     // 1-based
	Column   int    `json:"column"`   // 1-based
	Severity string `json:"severity"` // error | warning | info
	Code     string `json:"code"`     // "TS2322", "E0308", "no-unused-vars"
	Message  string `json:"message"`
	Checker  string `json:"checker"`
}

// The severities. Line and column are 1-based because every compiler in the set
// already reports that way; normalizing to 0-based would mean one translation
// per checker and one chance each to be off by one.
const (
	Error   = "error"
	Warning = "warning"
	Info    = "info"
)

// Options are the knobs that changed the measurements. Everything else a
// checker needs it reads from the project.
type Options struct {
	// Lib names the standard-library surface to parse. Empty takes es2022,
	// which is the difference between a 1.7s check and a 273ms one: the DOM and
	// ES .d.ts bundle is half the memory and most of the time on a small
	// project, and the project's own source is not the cost. A project that
	// declares its own libs is honored and this is ignored.
	Lib []string
	// Types names the ambient type packages to include. The default is none.
	Types []string
	// Memory is the guest's garbage-collection ceiling in MiB. Empty takes the
	// host's hard cap, and a value above it is refused rather than clamped.
	Memory uint32
}

// BundleOptions are a bundler's knobs.
type BundleOptions struct {
	Format    string // esm | cjs | iife; empty takes esm
	Target    string // es2022 by default
	Minify    bool
	Sourcemap bool
}

// File is one output file. Path is the name the bundler gave it under the
// project root.
type File struct {
	Path  string `json:"path"`
	Bytes []byte `json:"bytes"`
}

// Artifact is what a bundle produced. Diagnostics are normalized the same way a
// checker's are, so a bundle failure reads like a check failure; Bundle returns
// an error only when it could not run at all.
type Artifact struct {
	Files       []File       `json:"files"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

// Find returns the first output whose path ends in suffix.
func (a Artifact) Find(suffix string) (File, bool) {
	for _, f := range a.Files {
		if len(f.Path) >= len(suffix) && f.Path[len(f.Path)-len(suffix):] == suffix {
			return f, true
		}
	}
	return File{}, false
}

// Checker answers one question about a project in one language.
type Checker interface {
	// Name is the stable token an agent asks for: "tsgo", "oxlint", "clippy".
	Name() string
	// Detects reports whether this checker applies to the project as given — by
	// manifest, by config file, by extension. A checker that cannot tell
	// returns false rather than guessing.
	Detects(Project) bool
	// Check runs and returns normalized diagnostics. A checker that cannot run
	// returns an error naming what was missing, never an empty success.
	Check(context.Context, Project, Options) ([]Diagnostic, error)
}

// Bundler turns a project's entries into output files.
type Bundler interface {
	Name() string
	Detects(Project) bool
	Bundle(context.Context, Project, BundleOptions) (Artifact, error)
}

var registry struct {
	sync.RWMutex
	checkers map[string]Checker
	bundlers map[string]Bundler
}

// Register files v under its own name, as a checker if it checks, as a bundler
// if it bundles, as both if it does both. A value that does neither is refused,
// and so is a second value claiming a name already taken.
func Register(v any) error {
	c, isChecker := v.(Checker)
	b, isBundler := v.(Bundler)
	if !isChecker && !isBundler {
		return fmt.Errorf("compile: %T is neither a Checker nor a Bundler", v)
	}
	registry.Lock()
	defer registry.Unlock()
	if isChecker {
		if registry.checkers == nil {
			registry.checkers = map[string]Checker{}
		}
		if _, dup := registry.checkers[c.Name()]; dup {
			return fmt.Errorf("compile: checker %q already registered", c.Name())
		}
	}
	if isBundler {
		if registry.bundlers == nil {
			registry.bundlers = map[string]Bundler{}
		}
		if _, dup := registry.bundlers[b.Name()]; dup {
			return fmt.Errorf("compile: bundler %q already registered", b.Name())
		}
	}
	if isChecker {
		registry.checkers[c.Name()] = c
	}
	if isBundler {
		registry.bundlers[b.Name()] = b
	}
	return nil
}

// Unregister drops a name from both roles. It exists for a host that reloads a
// checker, and for tests that register one.
func Unregister(name string) {
	registry.Lock()
	defer registry.Unlock()
	delete(registry.checkers, name)
	delete(registry.bundlers, name)
}

// Checkers lists every registered checker by name.
func Checkers() []Checker {
	registry.RLock()
	defer registry.RUnlock()
	out := make([]Checker, 0, len(registry.checkers))
	for _, c := range registry.checkers {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// Bundlers lists every registered bundler by name.
func Bundlers() []Bundler {
	registry.RLock()
	defer registry.RUnlock()
	out := make([]Bundler, 0, len(registry.bundlers))
	for _, b := range registry.bundlers {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// CheckerNamed returns the checker registered under name.
func CheckerNamed(name string) (Checker, bool) {
	registry.RLock()
	defer registry.RUnlock()
	c, ok := registry.checkers[name]
	return c, ok
}

// BundlerNamed returns the bundler registered under name.
func BundlerNamed(name string) (Bundler, bool) {
	registry.RLock()
	defer registry.RUnlock()
	b, ok := registry.bundlers[name]
	return b, ok
}
