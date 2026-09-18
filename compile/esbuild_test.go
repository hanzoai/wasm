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
	b, err := NewEsbuild(ctx, Config{Blob: blob(t, envEsbuild, envEsbSum), Cache: cache(t)})
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
