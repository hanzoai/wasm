// Copyright © 2026 Hanzo AI. MIT License.

package compile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
)

// The modules are 20 and 49 MiB, so they are fetched rather than committed.
// Point these at a local build or a release; the digest is checked either way.
const (
	envTSGo    = "TSGO_WASM"
	envEsbuild = "ESBUILD_WASM"
	envTSGoSum = "TSGO_SHA256"
	envEsbSum  = "ESBUILD_SHA256"
)

func blob(t *testing.T, env, sumEnv string) Blob {
	t.Helper()
	path := os.Getenv(env)
	if path == "" {
		t.Skipf("%s is unset: no module to run", env)
	}
	sum := os.Getenv(sumEnv)
	if sum == "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		h := sha256.Sum256(raw)
		sum = hex.EncodeToString(h[:])
		t.Logf("%s=%s (computed; set %s to pin)", env, sum, sumEnv)
	}
	return Blob{Path: path, Sum: sum}
}

// cache keeps compiled machine code between runs of the test binary, which is
// the difference between 12s and 300ms per constructor.
func cache(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(os.TempDir(), "hanzo-compile-cache")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("cache dir: %v", err)
	}
	return dir
}

// project is two files and one deliberate type error.
func project() Project {
	return Project{
		Root:  "/project",
		Entry: []string{"entry.ts"},
		Files: Map{
			"tsconfig.json": []byte(`{"compilerOptions":{"strict":true,"noEmit":true,"target":"es2022","module":"esnext","moduleResolution":"bundler","lib":["es2022"],"types":[]},"files":["entry.ts","math.ts"]}`),
			"entry.ts":      []byte("import { add } from \"./math\";\nexport const wrong: string = add(1, 2);\n"),
			"math.ts":       []byte("export const add = (a: number, b: number): number => a + b;\n"),
		},
	}
}

type fake struct{ name string }

func (f fake) Name() string                                                  { return f.name }
func (f fake) Detects(Project) bool                                          { return true }
func (f fake) Check(context.Context, Project, Options) ([]Diagnostic, error) { return nil, nil }

func TestRegistry(t *testing.T) {
	defer Unregister("fake")
	defer Unregister("esbuild")

	if err := Register(fake{"fake"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := Register(fake{"fake"}); err == nil {
		t.Fatal("registering a name twice was accepted")
	}
	if err := Register(struct{}{}); err == nil {
		t.Fatal("registering a value that is neither was accepted")
	}

	// A nil *Esbuild is enough to register: Name does not touch the runtime,
	// and this asserts the roles without compiling 20 MiB.
	if err := Register((*Esbuild)(nil)); err != nil {
		t.Fatalf("register esbuild: %v", err)
	}
	if _, ok := BundlerNamed("esbuild"); !ok {
		t.Fatal("esbuild is not registered as a bundler")
	}
	if c, ok := CheckerNamed("esbuild"); ok {
		t.Fatalf("esbuild is registered as a checker (%T): it does not typecheck", c)
	}
	if _, isChecker := any((*Esbuild)(nil)).(Checker); isChecker {
		t.Fatal("*Esbuild satisfies Checker: it bundles a type error and exits 0")
	}
	if _, ok := CheckerNamed("fake"); !ok {
		t.Fatal("fake is not registered as a checker")
	}
	if _, ok := BundlerNamed("fake"); ok {
		t.Fatal("fake is registered as a bundler")
	}

	names := []string{}
	for _, c := range Checkers() {
		names = append(names, c.Name())
	}
	if len(names) != 1 || names[0] != "fake" {
		t.Fatalf("checkers = %v, want [fake]", names)
	}
	if b := Bundlers(); len(b) != 1 || b[0].Name() != "esbuild" {
		t.Fatalf("bundlers = %v, want [esbuild]", b)
	}
}

func TestBlobDigest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "module.wasm")
	body := []byte("\x00asm not really")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	ctx := context.Background()

	got, err := Blob{Path: path, Sum: hex.EncodeToString(sum[:])}.Bytes(ctx)
	if err != nil {
		t.Fatalf("matching digest was refused: %v", err)
	}
	if string(got) != string(body) {
		t.Fatal("bytes came back changed")
	}
	if _, err := (Blob{Path: path, Sum: "00"}).Bytes(ctx); err == nil {
		t.Fatal("a wrong digest was accepted")
	}
	if _, err := (Blob{Path: path}).Bytes(ctx); err == nil {
		t.Fatal("a module with no digest was accepted")
	}
	if _, err := (Blob{Sum: hex.EncodeToString(sum[:])}).Bytes(ctx); err == nil {
		t.Fatal("a blob naming nowhere was accepted")
	}
}

func TestMapEntries(t *testing.T) {
	m := Map{
		"tsconfig.json": []byte("{}"),
		"src/a.ts":      []byte("a"),
		"src/deep/b.ts": []byte("b"),
	}
	if !m.DirExists("src") || !m.DirExists(".") || m.DirExists("nope") {
		t.Fatal("DirExists disagrees with the keys")
	}
	if !m.FileExists("src/a.ts") || m.FileExists("src") {
		t.Fatal("FileExists disagrees with the keys")
	}
	top, err := m.Entries(".")
	if err != nil {
		t.Fatal(err)
	}
	if len(top) != 2 || top[0] != "src" || top[1] != "tsconfig.json" {
		t.Fatalf("root entries = %v", top)
	}
	inner, err := m.Entries("src")
	if err != nil {
		t.Fatal(err)
	}
	if len(inner) != 2 || inner[0] != "a.ts" || inner[1] != "deep" {
		t.Fatalf("src entries = %v", inner)
	}
	if _, err := m.Entries("nope"); err == nil {
		t.Fatal("listing a directory that does not exist succeeded")
	}
	if _, err := m.ReadFile("nope.ts"); err == nil {
		t.Fatal("reading a file that does not exist succeeded")
	}
	if p, err := m.Realpath("./src/a.ts"); err != nil || p != "src/a.ts" {
		t.Fatalf("Realpath = %q, %v", p, err)
	}
}

// The guest's whole filesystem is this adapter, so it has to behave like one.
func TestTreeIsAFilesystem(t *testing.T) {
	tr := &tree{
		files: Map{"src/a.ts": []byte("a"), "src/deep/b.ts": []byte("bb")},
		over:  map[string][]byte{"tsconfig.json": []byte("{}")},
	}
	if err := fstest.TestFS(tr, "tsconfig.json", "src/a.ts", "src/deep/b.ts"); err != nil {
		t.Fatalf("tree is not a valid fs.FS: %v", err)
	}
	if _, err := fs.Stat(tr, "src/missing.ts"); err == nil {
		t.Fatal("stat of a missing file succeeded")
	}
	b, err := fs.ReadFile(tr, "tsconfig.json")
	if err != nil || string(b) != "{}" {
		t.Fatalf("synthesized file = %q, %v", b, err)
	}
}
