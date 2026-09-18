// Copyright © 2026 Hanzo AI. MIT License.

package compile

import (
	"context"
	"strings"
	"sync"
	"testing"
)

func tsgo(t *testing.T) *TSGo {
	t.Helper()
	ctx := context.Background()
	c, err := NewTSGo(ctx, Config{Blob: blob(t, envTSGo, envTSGoSum), Cache: cache(t)})
	if err != nil {
		t.Fatalf("new tsgo: %v", err)
	}
	t.Cleanup(func() { c.Close(ctx) })
	return c
}

func TestTSGoDetects(t *testing.T) {
	c := (*TSGo)(nil)
	if !c.Detects(project()) {
		t.Fatal("a project with a tsconfig was not detected")
	}
	if !c.Detects(Project{Entry: []string{"a.ts"}, Files: Map{"a.ts": nil}}) {
		t.Fatal("a TypeScript entry was not detected")
	}
	if c.Detects(Project{Entry: []string{"main.go"}, Files: Map{"main.go": nil}}) {
		t.Fatal("a Go project was detected as TypeScript")
	}
	if c.Detects(Project{}) {
		t.Fatal("a project serving no files was detected")
	}
}

// The one diagnostic, and nothing else: file, 1-based line and column, code and
// severity all have to be right, because an agent acts on them without looking
// at the source.
func TestTSGoFindsTheTypeError(t *testing.T) {
	c := tsgo(t)
	diags, err := c.Check(context.Background(), project(), Options{})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(diags) != 1 {
		t.Fatalf("got %d diagnostics, want 1: %+v", len(diags), diags)
	}
	d := diags[0]
	want := Diagnostic{
		File: "entry.ts", Line: 2, Column: 14,
		Severity: Error, Code: "TS2322", Checker: "tsgo",
	}
	if d.File != want.File || d.Line != want.Line || d.Column != want.Column ||
		d.Severity != want.Severity || d.Code != want.Code || d.Checker != want.Checker {
		t.Fatalf("diagnostic = %+v, want %+v (message %q)", d, want, d.Message)
	}
	if !strings.Contains(d.Message, "not assignable") {
		t.Fatalf("message = %q", d.Message)
	}
}

// A project that says nothing about libs gets es2022 and no ambient types, from
// a tsconfig that exists only in the guest's view of the project.
func TestTSGoSynthesizesAConfig(t *testing.T) {
	c := tsgo(t)
	p := project()
	files := Map{}
	for k, v := range p.Files.(Map) {
		if k != "tsconfig.json" {
			files[k] = v
		}
	}
	p.Files = files
	p.Entry = []string{"entry.ts", "math.ts"}

	diags, err := c.Check(context.Background(), p, Options{})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(diags) != 1 || diags[0].Code != "TS2322" {
		t.Fatalf("diagnostics = %+v", diags)
	}
	if files.FileExists("tsconfig.json") {
		t.Fatal("the synthesized config leaked into the project")
	}
}

// A clean project is a clean answer, not an empty one that hides a crash.
func TestTSGoCleanProject(t *testing.T) {
	c := tsgo(t)
	p := project()
	p.Files.(Map)["entry.ts"] = []byte("import { add } from \"./math\";\nexport const right: number = add(1, 2);\n")

	diags, err := c.Check(context.Background(), p, Options{})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(diags) != 0 {
		t.Fatalf("diagnostics = %+v, want none", diags)
	}
}

// Eight instances of one compiled module, at once, each with its own linear
// memory. This is the shape a session-per-agent host runs in.
func TestTSGoFleet(t *testing.T) {
	const n = 8
	c := tsgo(t)
	var wg sync.WaitGroup
	errs := make([]error, n)
	counts := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			diags, err := c.Check(context.Background(), project(), Options{})
			errs[i], counts[i] = err, len(diags)
		}(i)
	}
	wg.Wait()
	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("check %d: %v", i, errs[i])
		}
		if counts[i] != 1 {
			t.Fatalf("check %d returned %d diagnostics, want 1", i, counts[i])
		}
	}
}

// A ceiling the linear memory cannot hold is refused before anything runs.
func TestTSGoRefusesMemoryAboveTheCap(t *testing.T) {
	c := tsgo(t)
	_, err := c.Check(context.Background(), project(), Options{Memory: 4096})
	if err == nil {
		t.Fatal("a 4 GiB ceiling was accepted under a 256 MiB cap")
	}
	if !strings.Contains(err.Error(), "host cap") {
		t.Fatalf("error does not name the cap: %v", err)
	}
}

func TestTSGoRefusesAnEmptyProject(t *testing.T) {
	c := tsgo(t)
	if _, err := c.Check(context.Background(), Project{}, Options{}); err == nil {
		t.Fatal("a project serving no files was checked")
	}
	if _, err := c.Check(context.Background(), Project{Files: Map{}}, Options{}); err == nil {
		t.Fatal("a project with no files and no config was checked")
	}
}

func TestParseTSC(t *testing.T) {
	out := "project/src/a.ts(3,17): error TS2322: Type 'number' is not assignable to type 'string'.\n" +
		"  The expected type comes from property 'x'.\n" +
		"error TS5083: Cannot read file '/project/tsconfig.json'.\n" +
		"src/b.ts(1,1): warning TS6133: 'x' is declared but never read.\n"
	diags := parseTSC(out, "/project")
	if len(diags) != 3 {
		t.Fatalf("got %d diagnostics: %+v", len(diags), diags)
	}
	if diags[0].File != "src/a.ts" || diags[0].Line != 3 || diags[0].Column != 17 || diags[0].Severity != Error {
		t.Fatalf("located = %+v", diags[0])
	}
	if !strings.HasSuffix(diags[0].Message, "property 'x'.") {
		t.Fatalf("continuation line was dropped: %q", diags[0].Message)
	}
	if diags[1].Code != "TS5083" || diags[1].File != "" || diags[1].Line != 0 {
		t.Fatalf("bare = %+v", diags[1])
	}
	if diags[2].Severity != Warning || diags[2].File != "src/b.ts" {
		t.Fatalf("warning = %+v", diags[2])
	}
}

// Options beat the project's own tsconfig, the way tsc's flags do: the same
// source that cannot find `document` under es2022 alone checks clean once dom
// is asked for.
func TestTSGoLibOverridesTheProject(t *testing.T) {
	c := tsgo(t)
	p := project()
	p.Files.(Map)["entry.ts"] = []byte("export const title: string = document.title;\n")
	p.Files.(Map)["tsconfig.json"] = []byte(`{"compilerOptions":{"strict":true,"noEmit":true,"target":"es2022","module":"esnext","moduleResolution":"bundler","lib":["es2022"],"types":[]},"files":["entry.ts"]}`)

	diags, err := c.Check(context.Background(), p, Options{})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(diags) != 1 || diags[0].Code != "TS2584" {
		t.Fatalf("es2022 alone gave %+v, want one TS2584", diags)
	}

	diags, err = c.Check(context.Background(), p, Options{Lib: []string{"es2022", "dom"}})
	if err != nil {
		t.Fatalf("check with dom: %v", err)
	}
	if len(diags) != 0 {
		t.Fatalf("with dom asked for: %+v, want none", diags)
	}
}

// A cancelled context ends the check as a cancellation, not as a mystery exit
// code.
func TestTSGoRespectsCancellation(t *testing.T) {
	c := tsgo(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Check(ctx, project(), Options{}); err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
