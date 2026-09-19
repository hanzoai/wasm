// Copyright © 2026 Hanzo AI. MIT License.

package compile

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"strings"
	"sync"
	"testing"
)

// raw serves names exactly as asked and remembers every one. Map cleans a name
// before looking it up, which normalizes ".." away and is why a shipped test
// cannot see a guest climbing out of the project; this one answers for
// "../outside.ts" if anything asks, the way a host reading
// filepath.Join(base, name) off a disk would.
type raw struct {
	mu    sync.Mutex
	files map[string][]byte
	asked []string
	alias map[string]string
}

var _ Files = (*raw)(nil)

func (r *raw) note(op, name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.asked = append(r.asked, op+" "+name)
}

// escaped returns the names asked for that do not stay inside the project.
func (r *raw) escaped() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, a := range r.asked {
		if name := a[strings.IndexByte(a, ' ')+1:]; !fs.ValidPath(name) {
			out = append(out, a)
		}
	}
	return out
}

func (r *raw) ReadFile(name string) ([]byte, error) {
	r.note("read", name)
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.files[name]
	if !ok {
		return nil, fmt.Errorf("no file %q", name)
	}
	return b, nil
}

func (r *raw) FileExists(name string) bool {
	r.note("stat", name)
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.files[name]
	return ok
}

func (r *raw) DirExists(name string) bool {
	r.note("statdir", name)
	r.mu.Lock()
	defer r.mu.Unlock()
	if name == "." || name == "" {
		return true
	}
	for k := range r.files {
		if strings.HasPrefix(k, name+"/") {
			return true
		}
	}
	return false
}

func (r *raw) Entries(name string) ([]string, error) {
	r.note("list", name)
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for k := range r.files {
		if !strings.ContainsRune(k, '/') && name == "." {
			out = append(out, k)
		}
	}
	return out, nil
}

func (r *raw) Realpath(name string) (string, error) {
	r.note("real", name)
	r.mu.Lock()
	defer r.mu.Unlock()
	if to, ok := r.alias[name]; ok {
		return to, nil
	}
	if _, ok := r.files[name]; ok {
		return name, nil
	}
	if name == "." {
		return name, nil
	}
	return "", fmt.Errorf("no such name %q", name)
}

const secret = "HOST-SECRET-9f3a"

// A guest that imports its way out of the project root reaches nothing. The
// import is spelled absolutely so it takes the one branch of the resolver that
// does no joining, and the host answers for the file outside — so if the name
// ever arrives, the bytes come back.
func TestEsbuildRefusesAnImportOutsideTheProject(t *testing.T) {
	b := esbuild(t)
	files := &raw{files: map[string][]byte{
		"entry.ts":      []byte("import { leaked } from \"/project/../outside\";\nexport const x = leaked;\n"),
		"../outside.ts": []byte("export const leaked = \"" + secret + "\";\n"),
	}}
	p := Project{Root: "/project", Entry: []string{"entry.ts"}, Files: files}

	art, err := b.Bundle(context.Background(), p, BundleOptions{})
	if err != nil {
		t.Fatalf("bundle: %v", err)
	}
	if out := files.escaped(); len(out) > 0 {
		t.Fatalf("the host was asked for names outside the project: %v", out)
	}
	for _, f := range art.Files {
		if strings.Contains(string(f.Bytes), secret) {
			t.Fatalf("a file outside the project was inlined into %s:\n%s", f.Path, f.Bytes)
		}
	}
	// The name does not exist as far as the guest is concerned, and the guest is
	// told so at the line that asked for it — not left to bundle nothing and
	// wonder.
	if len(art.Diagnostics) != 1 {
		t.Fatalf("diagnostics = %+v, want one", art.Diagnostics)
	}
	if d := art.Diagnostics[0]; d.File != "entry.ts" || d.Line != 1 || d.Severity != Error {
		t.Fatalf("diagnostic = %+v", d)
	}
	if !strings.Contains(art.Diagnostics[0].Message, "/project/../outside") {
		t.Fatalf("the diagnostic does not name the import: %q", art.Diagnostics[0].Message)
	}
}

// An entry is the one name a caller supplies rather than a guest, and it gets
// the same treatment.
func TestEsbuildRefusesAnEntryOutsideTheProject(t *testing.T) {
	files := &raw{files: map[string][]byte{"../../../../etc/passwd.ts": []byte("export const x = 1;\n")}}
	p := Project{Root: "/project", Entry: []string{"../../../../etc/passwd.ts"}, Files: files}

	b := (*Esbuild)(nil)
	if b.Detects(p) {
		t.Error("an entry outside the project was detected")
	}
	if _, err := esbuild(t).Bundle(context.Background(), p, BundleOptions{}); err == nil {
		t.Error("an entry outside the project was bundled")
	}
	if out := files.escaped(); len(out) > 0 {
		t.Fatalf("the host was asked for names outside the project: %v", out)
	}
}

// The path in an on-load request is the guest's word for which file to read. It
// is checked against the names a resolve actually answered for, because a
// request field is not authority.
func TestLoadRefusesWhatNoResolveAnswered(t *testing.T) {
	files := &raw{files: map[string][]byte{
		"entry.ts":            []byte("export const x = 1;\n"),
		"../../etc/passwd.ts": []byte("export const leaked = \"" + secret + "\";\n"),
		"served.ts":           []byte("export const served = 1;\n"),
	}}
	h := &plugin{files: files, root: "/project"}

	// The extension is a .ts on purpose: a name this bundler has no loader for
	// is refused a step earlier and would prove nothing about who decides which
	// file gets read.
	for _, ask := range []string{"/project/../../etc/passwd.ts", "/project/served.ts"} {
		resp := h.answer(map[string]any{"command": "on-load", "path": ask})
		if msg, _ := resp["error"].(string); msg == "" {
			t.Errorf("a load of %q, which no resolve answered for, returned %v", ask, resp)
		}
		if src, ok := resp["contents"].([]byte); ok {
			t.Errorf("a load of %q returned %d bytes", ask, len(src))
		}
	}
	if h.failed() == nil {
		t.Error("an unresolved load did not fail the build")
	}
	if len(files.asked) > 0 {
		t.Fatalf("the host was asked for anything at all: %v", files.asked)
	}

	// The same name, once a resolve has answered for it, loads.
	if _, ok := h.serves("/project/served.ts"); !ok {
		t.Fatal("a file the project serves did not resolve")
	}
	resp := h.answer(map[string]any{"command": "on-load", "path": "/project/served.ts"})
	if src, _ := resp["contents"].([]byte); !bytes.Contains(src, []byte("served = 1")) {
		t.Fatalf("a resolved file did not load: %v", resp)
	}
}

// Realpath is the host's answer, and the host is trusted for bytes — not for
// staying inside its own project. A name that leaves is refused rather than
// read.
func TestTreeRefusesAnAliasOutOfTheProject(t *testing.T) {
	files := &raw{
		files: map[string][]byte{"a.ts": []byte("export const a = 1;\n")},
		alias: map[string]string{"a.ts": "../../../../etc/passwd"},
	}
	tr := &tree{files: files}
	if b, ok := tr.read("a.ts"); ok {
		t.Fatalf("an aliased name out of the project was read: %q", b)
	}
	if out := files.escaped(); len(out) > 0 {
		t.Fatalf("the host was asked for names outside the project: %v", out)
	}
}

func TestRelRefusesWhatLeavesTheProject(t *testing.T) {
	for _, c := range []struct{ name, root, want string }{
		{"/project/entry.ts", "/project", "entry.ts"},
		{"project/src/a.ts", "/project", "src/a.ts"},
		{"src/a.ts", "/project", "src/a.ts"},
		{"./entry.ts", "/project", "entry.ts"},
		{"/project/out/./entry.js", "/project", "out/entry.js"},
		{"/project/../outside.ts", "/project", ""},
		{"../../../../etc/passwd", "/project", ""},
		{"/etc/passwd", "/project", ""},
		{"/project", "/project", ""},
		{"/project/", "/project", ""},
		{"", "/project", ""},
	} {
		got, err := rel(c.name, c.root)
		if c.want == "" {
			if err == nil {
				t.Errorf("rel(%q, %q) = %q, want a refusal", c.name, c.root, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("rel(%q, %q) = %q, %v; want %q", c.name, c.root, got, err, c.want)
		}
	}
}
