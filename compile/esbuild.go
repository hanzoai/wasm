// Copyright © 2026 Hanzo AI. MIT License.

package compile

import (
	"context"
	"fmt"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/sys"
)

// The protocol version the module must answer with. esbuild refuses a client it
// does not match, and so do we: a bundler that silently spoke an older protocol
// would disagree about flags rather than fail.
const esbuildVersion = "0.28.2"

// hostspace is the namespace every file the host serves lives in. esbuild
// prefixes a diagnostic's file with it, so it is also what has to come off
// again on the way out.
const hostspace = "hostfs"

// Esbuild bundles. The module is upstream esbuild built for wasip1, driven over
// its stdio service protocol with a plugin standing in for the filesystem, so
// it runs with zero preopened directories: every import is resolved and loaded
// by the host, and a file the host does not answer for does not exist.
//
// It is a Bundler and NOT a Checker. It bundled `const wrong: string =
// add(1,2)` and exited 0 — it parses types and discards them. Registering it as
// a checker would make the registry lie about what has been verified.
type Esbuild struct {
	e *engine
}

var _ Bundler = (*Esbuild)(nil)

// NewEsbuild compiles the module once.
func NewEsbuild(ctx context.Context, c Config) (*Esbuild, error) {
	e, err := newEngine(ctx, c)
	if err != nil {
		return nil, err
	}
	return &Esbuild{e: e}, nil
}

// Close releases the runtime and the compiled module.
func (b *Esbuild) Close(ctx context.Context) error { return b.e.Close(ctx) }

// Name is the token an agent asks for.
func (b *Esbuild) Name() string { return "esbuild" }

// loaders is the whole set of inputs this bundler claims. An extension that is
// not here is refused by name rather than loaded as text, because a wrong
// loader produces a bundle that looks fine and runs wrong.
var loaders = map[string]string{
	".ts": "ts", ".tsx": "tsx", ".mts": "ts", ".cts": "ts",
	".js": "js", ".jsx": "jsx", ".mjs": "js", ".cjs": "js",
	".json": "json", ".css": "css", ".txt": "text",
}

// Detects reports a project with at least one entry this bundler can load.
func (b *Esbuild) Detects(p Project) bool {
	if p.Files == nil {
		return false
	}
	for _, e := range p.Entry {
		if _, ok := loaders[path.Ext(e)]; ok && p.Files.FileExists(e) {
			return true
		}
	}
	return false
}

// Bundle runs one instance and returns its output files.
//
// Diagnostics are normalized the same way a checker's are; an error means the
// bundler could not run, not that the source was wrong.
func (b *Esbuild) Bundle(ctx context.Context, p Project, o BundleOptions) (Artifact, error) {
	if p.Files == nil {
		return Artifact{}, fmt.Errorf("compile: esbuild: project serves no files")
	}
	if len(p.Entry) == 0 {
		return Artifact{}, fmt.Errorf("compile: esbuild: project names no entry")
	}
	root := strings.TrimSuffix(p.root(), "/")
	for _, e := range p.Entry {
		if _, ok := loaders[path.Ext(e)]; !ok {
			return Artifact{}, fmt.Errorf("compile: esbuild: no loader for %q", path.Ext(e))
		}
		if !p.Files.FileExists(e) {
			return Artifact{}, fmt.Errorf("compile: esbuild: entry %q is not in the project", e)
		}
	}

	// Real pipes, not io.Pipe: the guest polls its stdin, which needs a
	// descriptor the host runtime can wait on.
	guestIn, hostW, err := os.Pipe()
	if err != nil {
		return Artifact{}, fmt.Errorf("compile: esbuild: pipe: %w", err)
	}
	hostR, guestOut, err := os.Pipe()
	if err != nil {
		guestIn.Close()
		hostW.Close()
		return Artifact{}, fmt.Errorf("compile: esbuild: pipe: %w", err)
	}
	defer func() {
		hostW.Close()
		hostR.Close()
	}()

	cfg := wazero.NewModuleConfig().
		WithName(b.e.name("esbuild")).
		WithArgs("esbuild", "--service="+esbuildVersion).
		WithStdin(guestIn).WithStdout(guestOut).WithStderr(os.Stderr).
		WithSysWalltime().WithSysNanotime().WithSysNanosleep()

	done := make(chan error, 1)
	go func() {
		mod, err := b.e.rt.InstantiateModule(ctx, b.e.mod, cfg)
		if mod != nil {
			_ = mod.Close(ctx)
		}
		guestOut.Close()
		guestIn.Close()
		if exit, ok := err.(*sys.ExitError); ok && exit.ExitCode() == 0 {
			err = nil
		}
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		done <- err
	}()

	host := &hostfs{files: p.Files, root: root}
	s := newService(hostW, hostR)
	s.answer = host.answer
	if err := s.handshake(); err != nil {
		return Artifact{}, err
	}
	if s.version != esbuildVersion {
		return Artifact{}, fmt.Errorf("compile: esbuild module is %s, host speaks %s", s.version, esbuildVersion)
	}
	go s.serve()

	resp, err := s.send(buildRequest(root, p.Entry, o))
	if err != nil {
		return Artifact{}, err
	}
	if msg, ok := resp["error"].(string); ok && msg != "" {
		return Artifact{}, fmt.Errorf("compile: esbuild refused the build: %s", msg)
	}

	art := artifact(resp, root)
	if err := host.failed(); err != nil {
		return art, err
	}

	// Closing the write end is how the service is told to exit; a module that
	// then hangs is a defect worth naming rather than a goroutine to leak.
	hostW.Close()
	select {
	case err := <-done:
		if err != nil {
			return art, fmt.Errorf("compile: esbuild: %w", err)
		}
	case <-time.After(5 * time.Second):
		return art, fmt.Errorf("compile: esbuild did not exit after its stdin closed")
	case <-ctx.Done():
		return art, ctx.Err()
	}
	return art, nil
}

func buildRequest(root string, entry []string, o BundleOptions) map[string]any {
	format := o.Format
	if format == "" {
		format = "esm"
	}
	target := o.Target
	if target == "" {
		target = "es2022"
	}
	flags := []any{
		"--bundle",
		"--format=" + format,
		"--target=" + target,
		"--outdir=" + root + "/out",
	}
	if o.Minify {
		flags = append(flags, "--minify")
	}
	if o.Sourcemap {
		flags = append(flags, "--sourcemap")
	}
	entries := make([]any, 0, len(entry))
	for _, e := range entry {
		entries = append(entries, []any{"", root + "/" + e})
	}
	return map[string]any{
		"command":       "build",
		"key":           0,
		"context":       false,
		"write":         false,
		"entries":       entries,
		"flags":         flags,
		"absWorkingDir": root,
		"nodePaths":     []any{},
		"plugins": []any{map[string]any{
			"name":      hostspace,
			"onEnd":     false,
			"onResolve": []any{map[string]any{"id": 1, "filter": ".*", "namespace": ""}},
			"onLoad":    []any{map[string]any{"id": 2, "filter": ".*", "namespace": hostspace}},
		}},
	}
}

// hostfs answers the guest's resolve and load requests out of the project. This
// is the filesystem, and it is a function call.
//
// Answers run on the goroutine reading the guest's stream while the caller
// waits on the build, so the one field that crosses between them is guarded.
type hostfs struct {
	files Files
	root  string

	mu     sync.Mutex
	broken error
}

func (h *hostfs) fail(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.broken == nil {
		h.broken = err
	}
}

func (h *hostfs) failed() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.broken
}

// extensions are tried in the order TypeScript's own resolver tries them.
var extensions = []string{"", ".ts", ".tsx", ".mts", ".cts", ".js", ".jsx", ".mjs", ".cjs", ".json", ".css"}

func (h *hostfs) answer(req map[string]any) map[string]any {
	switch req["command"] {
	case "on-start":
		// Both keys are required: esbuild asserts them without checking, so an
		// empty answer panics the guest rather than failing the build.
		return map[string]any{"errors": []any{}, "warnings": []any{}}
	case "on-resolve":
		p, _ := req["path"].(string)
		importer, _ := req["importer"].(string)
		dir, _ := req["resolveDir"].(string)
		full, ok := h.resolve(p, importer, dir)
		if !ok {
			return map[string]any{"error": fmt.Sprintf("host serves no %q imported from %q", p, importer)}
		}
		return map[string]any{"id": 1, "path": full, "namespace": hostspace}
	case "on-load":
		full, _ := req["path"].(string)
		name := rel(full, h.root)
		loader, ok := loaders[path.Ext(name)]
		if !ok {
			return map[string]any{"error": fmt.Sprintf("host has no loader for %q", path.Ext(name))}
		}
		src, err := h.files.ReadFile(name)
		if err != nil {
			return map[string]any{"error": err.Error()}
		}
		return map[string]any{
			"id":         2,
			"loader":     loader,
			"contents":   src,
			"resolveDir": path.Dir(full),
		}
	case "ping":
		return map[string]any{}
	}
	err := fmt.Errorf("compile: esbuild asked for %v, which the host does not implement", req["command"])
	h.fail(err)
	return map[string]any{"error": err.Error()}
}

// resolve turns an import into a name the project answers for, or reports that
// nothing does. It never guesses: every candidate is checked against the host.
func (h *hostfs) resolve(p, importer, resolveDir string) (string, bool) {
	base := resolveDir
	if importer != "" {
		base = path.Dir(importer)
	}
	if base == "" {
		base = h.root
	}
	candidate := p
	switch {
	case strings.HasPrefix(p, "/"):
	case strings.HasPrefix(p, "."):
		candidate = path.Join(base, p)
	default:
		candidate = path.Join(h.root, p)
	}
	for _, ext := range extensions {
		if name, ok := h.serves(candidate + ext); ok {
			return name, true
		}
	}
	for _, index := range []string{"/index.ts", "/index.tsx", "/index.js", "/index.jsx"} {
		if name, ok := h.serves(candidate + index); ok {
			return name, true
		}
	}
	return "", false
}

// serves reports whether the project answers for a candidate, under the name
// the host itself uses. Two imports that reach one file through different names
// resolve to the same module, so the bundler includes it once.
func (h *hostfs) serves(candidate string) (string, bool) {
	name := rel(candidate, h.root)
	if !h.files.FileExists(name) {
		return "", false
	}
	if real, err := h.files.Realpath(name); err == nil {
		name = real
	}
	return h.root + "/" + name, true
}

func artifact(resp map[string]any, root string) Artifact {
	var art Artifact
	for _, f := range list(resp["outputFiles"]) {
		f, ok := f.(map[string]any)
		if !ok {
			continue
		}
		p, _ := f["path"].(string)
		bytes, _ := f["contents"].([]byte)
		art.Files = append(art.Files, File{Path: rel(p, root), Bytes: bytes})
	}
	art.Diagnostics = append(art.Diagnostics, messages(resp["errors"], Error, root)...)
	art.Diagnostics = append(art.Diagnostics, messages(resp["warnings"], Warning, root)...)
	return art
}

func list(v any) []any {
	a, _ := v.([]any)
	return a
}

// messages normalizes esbuild's shape. Its column is a 0-based byte offset into
// the line, so it is the one number that has to be translated: everything a
// caller sees from this package is 1-based.
func messages(v any, severity, root string) []Diagnostic {
	var out []Diagnostic
	for _, m := range list(v) {
		m, ok := m.(map[string]any)
		if !ok {
			continue
		}
		d := Diagnostic{Severity: severity, Checker: "esbuild"}
		d.Message, _ = m["text"].(string)
		d.Code, _ = m["id"].(string)
		if loc, ok := m["location"].(map[string]any); ok {
			file, _ := loc["file"].(string)
			file = strings.TrimPrefix(file, hostspace+":")
			line, _ := loc["line"].(int)
			column, _ := loc["column"].(int)
			d.File = rel(file, root)
			d.Line = line
			d.Column = column + 1
		}
		for _, n := range list(m["notes"]) {
			if n, ok := n.(map[string]any); ok {
				if text, _ := n["text"].(string); text != "" {
					d.Message += " " + text
				}
			}
		}
		out = append(out, d)
	}
	return out
}
