// Copyright © 2026 Hanzo AI. MIT License.

package compile

import (
	"context"
	"strings"
	"testing"
)

func esbuild(t *testing.T) *Esbuild {
	t.Helper()
	ctx := context.Background()
	b, err := NewEsbuild(ctx, Config{Blob: blob(t, envEsbuild, envEsbSum), Cache: cacheDir})
	if err != nil {
		t.Fatalf("new esbuild: %v", err)
	}
	t.Cleanup(func() { b.Close(ctx) })
	return b
}

// three is an entry and two modules it imports.
func three() Project {
	return Project{
		Root:  "/project",
		Entry: []string{"entry.ts"},
		Files: Map{
			"entry.ts": []byte("import { add, mul } from \"./math\";\nimport { greet } from \"./greet\";\nexport const main = (): string => greet(`${add(1, 2)}:${mul(3, 4)}`);\n"),
			"math.ts":  []byte("export const add = (a: number, b: number): number => a + b;\nexport const mul = (a: number, b: number): number => a * b;\n"),
			"greet.ts": []byte("export const greet = (who: string): string => `hello ${who}`;\n"),
		},
	}
}

func TestEsbuildDetects(t *testing.T) {
	b := (*Esbuild)(nil)
	if !b.Detects(three()) {
		t.Fatal("a TypeScript entry was not detected")
	}
	if b.Detects(Project{Entry: []string{"main.rs"}, Files: Map{"main.rs": nil}}) {
		t.Fatal("a Rust entry was detected")
	}
	if b.Detects(Project{Files: Map{"entry.ts": nil}}) {
		t.Fatal("a project naming no entry was detected")
	}
	if b.Detects(Project{Entry: []string{"gone.ts"}, Files: Map{}}) {
		t.Fatal("an entry the project does not serve was detected")
	}
}

// Three modules in, one bundle out, with every module's code in it — and the
// guest had no filesystem: every byte it read came back through onResolve and
// onLoad.
func TestEsbuildBundlesThreeModules(t *testing.T) {
	b := esbuild(t)
	art, err := b.Bundle(context.Background(), three(), BundleOptions{})
	if err != nil {
		t.Fatalf("bundle: %v", err)
	}
	if len(art.Diagnostics) != 0 {
		t.Fatalf("diagnostics = %+v", art.Diagnostics)
	}
	if len(art.Files) != 1 {
		t.Fatalf("got %d output files, want 1: %+v", len(art.Files), art.Files)
	}
	f := art.Files[0]
	if f.Path != "out/entry.js" {
		t.Fatalf("output path = %q", f.Path)
	}
	js := string(f.Bytes)
	for _, want := range []string{"a + b", "a * b", "hello ", "main"} {
		if !strings.Contains(js, want) {
			t.Fatalf("bundle is missing %q:\n%s", want, js)
		}
	}
	if strings.Contains(js, "import") || strings.Contains(js, "require(") {
		t.Fatalf("bundle still has module boundaries:\n%s", js)
	}
}

func TestEsbuildMinifiesAndMaps(t *testing.T) {
	b := esbuild(t)
	art, err := b.Bundle(context.Background(), three(), BundleOptions{Minify: true, Sourcemap: true})
	if err != nil {
		t.Fatalf("bundle: %v", err)
	}
	if len(art.Files) != 2 {
		t.Fatalf("got %d files, want js and map: %+v", len(art.Files), art.Files)
	}
	js, ok := art.Find("entry.js")
	if !ok {
		t.Fatalf("no js output: %+v", art.Files)
	}
	if _, ok := art.Find(".map"); !ok {
		t.Fatalf("no sourcemap: %+v", art.Files)
	}
	if strings.Contains(string(js.Bytes), "const add") {
		t.Fatalf("output is not minified:\n%s", js.Bytes)
	}
}

// esbuild parses types and throws them away. It is a Bundler for exactly this
// reason: the registry must not claim this source was checked.
func TestEsbuildDoesNotTypecheck(t *testing.T) {
	b := esbuild(t)
	p := three()
	p.Files.(Map)["entry.ts"] = []byte("import { add } from \"./math\";\nexport const wrong: string = add(1, 2);\n")

	art, err := b.Bundle(context.Background(), p, BundleOptions{})
	if err != nil {
		t.Fatalf("bundle: %v", err)
	}
	if len(art.Diagnostics) != 0 {
		t.Fatalf("esbuild reported the type error after all: %+v", art.Diagnostics)
	}
	if len(art.Files) != 1 {
		t.Fatalf("got %d output files, want 1", len(art.Files))
	}
}

// A missing import is the host's answer, not a filesystem's: the resolver asked
// the project and the project said no.
func TestEsbuildReportsAMissingImport(t *testing.T) {
	b := esbuild(t)
	p := three()
	p.Files.(Map)["entry.ts"] = []byte("import { nope } from \"./gone\";\nexport const x = nope;\n")

	art, err := b.Bundle(context.Background(), p, BundleOptions{})
	if err != nil {
		t.Fatalf("bundle: %v", err)
	}
	if len(art.Diagnostics) == 0 {
		t.Fatal("a missing import produced no diagnostic")
	}
	d := art.Diagnostics[0]
	if d.Severity != Error || d.Checker != "esbuild" {
		t.Fatalf("diagnostic = %+v", d)
	}
	if d.File != "entry.ts" || d.Line != 1 {
		t.Fatalf("location = %+v", d)
	}
	// esbuild points at the opening quote of the module path, counting bytes
	// from 0; a caller of this package counts from 1.
	if d.Column != 22 {
		t.Fatalf("column = %d, want 22 (1-based; esbuild said 21)", d.Column)
	}
	if !strings.Contains(d.Message, "gone") {
		t.Fatalf("message = %q", d.Message)
	}
}

// One position, one number, whichever compiler answered. esbuild counts bytes
// and tsgo counts UTF-16 code units, so a line with three two-byte runes in it
// is where a package that promises one diagnostic shape either keeps that
// promise or does not: the opening quote below is column 29 to tsgo and, before
// the translation, 32 to esbuild.
func TestColumnsAgreeAcrossCheckers(t *testing.T) {
	const src = "import { nope as ééé } from \"./gone\";\nexport const x = ééé;\n"
	const want = 29

	diags, err := tsgo(t).Check(context.Background(), Project{
		Root:  "/project",
		Entry: []string{"entry.ts"},
		Files: Map{"entry.ts": []byte(src)},
	}, Options{})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(diags) == 0 {
		t.Fatal("tsgo reported nothing about a module that is not there")
	}
	if diags[0].Line != 1 || diags[0].Column != want {
		t.Errorf("tsgo said %d:%d, want 1:%d", diags[0].Line, diags[0].Column, want)
	}

	art, err := esbuild(t).Bundle(context.Background(), Project{
		Root:  "/project",
		Entry: []string{"entry.ts"},
		Files: Map{"entry.ts": []byte(src)},
	}, BundleOptions{})
	if err != nil {
		t.Fatalf("bundle: %v", err)
	}
	if len(art.Diagnostics) == 0 {
		t.Fatal("esbuild reported nothing about a module that is not there")
	}
	if got := art.Diagnostics[0]; got.Line != 1 || got.Column != want {
		t.Errorf("esbuild said %d:%d, want 1:%d", got.Line, got.Column, want)
	}
}

// Two names for one file are one module. The host says so through Realpath, and
// nothing else can: only the host knows its own aliases.
func TestEsbuildBundlesAnAliasedFileOnce(t *testing.T) {
	b := esbuild(t)
	files := &raw{
		files: map[string][]byte{
			"entry.ts": []byte("import { mark } from \"./lib\";\nimport { mark as same } from \"./alias\";\nexport const x = mark + same;\n"),
			"lib.ts":   []byte("export const mark = 42;\n"),
			"alias.ts": []byte("export const mark = 42;\n"),
		},
		alias: map[string]string{"alias.ts": "lib.ts"},
	}
	art, err := b.Bundle(context.Background(), Project{Root: "/project", Entry: []string{"entry.ts"}, Files: files}, BundleOptions{})
	if err != nil {
		t.Fatalf("bundle: %v", err)
	}
	if len(art.Diagnostics) != 0 {
		t.Fatalf("diagnostics = %+v", art.Diagnostics)
	}
	f, ok := art.Find("entry.js")
	if !ok {
		t.Fatalf("no js output: %+v", art.Files)
	}
	if n := strings.Count(string(f.Bytes), "= 42"); n != 1 {
		t.Fatalf("the aliased module is in the bundle %d times:\n%s", n, f.Bytes)
	}
}

// A cancelled context ends the bundle as a cancellation, not as a truncated
// handshake with the module that was closed under it.
func TestEsbuildRespectsCancellation(t *testing.T) {
	b := esbuild(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := b.Bundle(ctx, three(), BundleOptions{}); err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// The registry with the real value in it, not a nil standing in for one.
func TestEsbuildRegistersOnlyAsABundler(t *testing.T) {
	b := esbuild(t)
	if err := Register(b); err != nil {
		t.Fatalf("register: %v", err)
	}
	defer Unregister(b.Name())
	if _, ok := BundlerNamed("esbuild"); !ok {
		t.Error("esbuild is not registered as a bundler")
	}
	if c, ok := CheckerNamed("esbuild"); ok {
		t.Errorf("esbuild is registered as a checker (%T): it does not typecheck", c)
	}
}

func TestEsbuildRefusesWhatItCannotLoad(t *testing.T) {
	b := esbuild(t)
	p := Project{Root: "/project", Entry: []string{"main.rs"}, Files: Map{"main.rs": []byte("fn main() {}")}}
	if _, err := b.Bundle(context.Background(), p, BundleOptions{}); err == nil {
		t.Fatal("a Rust entry was bundled")
	}
	p = Project{Root: "/project", Entry: []string{"gone.ts"}, Files: Map{}}
	if _, err := b.Bundle(context.Background(), p, BundleOptions{}); err == nil {
		t.Fatal("an entry the project does not serve was bundled")
	}
	if _, err := b.Bundle(context.Background(), Project{Files: Map{}}, BundleOptions{}); err == nil {
		t.Fatal("a project naming no entry was bundled")
	}
}
