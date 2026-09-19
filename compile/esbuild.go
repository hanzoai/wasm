// Copyright © 2026 Hanzo AI. MIT License.

package compile

import (
	"context"
	"fmt"
	"os"
	"path"
	"strings"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/sys"
)

// The protocol version the module must answer with. esbuild refuses a client it
// does not match, and so do we: a bundler that silently spoke an older protocol
// would disagree about flags rather than fail.
const esbuildVersion = "0.28.2"

// space is the namespace every file the host serves lives in, and the name of
// the plugin that answers for it. esbuild prefixes a diagnostic's file with it,
// so it is also what has to come off again on the way out.
const space = "project"

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
	names, err := p.names()
	if err != nil {
		return false
	}
	for _, e := range names {
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
	entry, err := p.names()
	if err != nil {
		return Artifact{}, err
	}
	for _, e := range entry {
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

	var errs sink
	cfg := wazero.NewModuleConfig().
		WithName(b.e.name("esbuild")).
		WithArgs("esbuild", "--service="+esbuildVersion).
		WithStdin(guestIn).WithStdout(guestOut).WithStderr(&errs).
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

	host := &plugin{files: p.Files, root: root}
	s := newService(hostW, hostR)
	s.answer = host.answer
	if err := s.handshake(); err != nil {
		// A cancelled context closes the instance, which ends the stream, which
		// reads as a truncated header. Report what actually happened.
		if ctx.Err() != nil {
			return Artifact{}, ctx.Err()
		}
		return Artifact{}, errs.wrap(err)
	}
	if s.version != esbuildVersion {
		return Artifact{}, fmt.Errorf("compile: esbuild module is %s, host speaks %s", s.version, esbuildVersion)
	}
	go s.serve()

	resp, err := s.send(buildRequest(root, entry, o))
	if err != nil {
		if ctx.Err() != nil {
			return Artifact{}, ctx.Err()
		}
		return Artifact{}, errs.wrap(err)
	}
	if msg, ok := resp["error"].(string); ok && msg != "" {
		return Artifact{}, fmt.Errorf("compile: esbuild refused the build: %s", msg)
	}

	art, err := artifact(resp, root)
	if err != nil {
		return Artifact{}, err
	}
	if err := host.failed(); err != nil {
		return art, err
	}

	// Closing the write end is how the service is told to exit. The wait is
	// bounded by the caller's context and nothing else: wazero closes the
	// instance when that context is done, which is the same bound the check path
	// runs under, and a constant here turns a slow box into a failed bundle.
	hostW.Close()
	select {
	case err := <-done:
		if err != nil {
			return art, errs.wrap(fmt.Errorf("compile: esbuild: %w", err))
		}
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
			"name":      space,
			"onEnd":     false,
			"onResolve": []any{map[string]any{"id": 1, "filter": ".*", "namespace": ""}},
			"onLoad":    []any{map[string]any{"id": 2, "filter": ".*", "namespace": space}},
		}},
	}
}

// plugin is the host half of esbuild's plugin protocol: it answers the guest's
// resolve and load requests out of the project. This is the filesystem, and it
// is a function call.
//
// Answers run on the goroutine reading the guest's stream while the caller waits
// on the build, so what crosses between them is guarded. served is why a load is
// not the guest's decision: a path arrives in a request field, and a request
// field is not authority — only a name some resolve already answered for is.
type plugin struct {
	files Files
	root  string

	mu     sync.Mutex
	broken error
	served map[string]string // project-relative name, by the path a resolve answered with
}

func (h *plugin) fail(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.broken == nil {
		h.broken = err
	}
}

func (h *plugin) failed() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.broken
}

func (h *plugin) keep(full, name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.served == nil {
		h.served = map[string]string{}
	}
	h.served[full] = name
}

func (h *plugin) kept(full string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	name, ok := h.served[full]
	return name, ok
}

// extensions are tried in the order TypeScript's own resolver tries them.
var extensions = []string{"", ".ts", ".tsx", ".mts", ".cts", ".js", ".jsx", ".mjs", ".cjs", ".json", ".css"}

func (h *plugin) answer(req map[string]any) map[string]any {
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
		return map[string]any{"id": 1, "path": full, "namespace": space}
	case "on-load":
		full, _ := req["path"].(string)
		name, ok := h.kept(full)
		if !ok {
			// The guest is asking for a file by a name no resolve of ours
			// produced. Reading it would make the request field the authority
			// on what the project contains.
			err := fmt.Errorf("compile: esbuild asked to load %q, which no resolve answered for", full)
			h.fail(err)
			return map[string]any{"error": err.Error()}
		}
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
func (h *plugin) resolve(p, importer, resolveDir string) (string, bool) {
	base := resolveDir
	if importer != "" {
		base = path.Dir(importer)
	}
	if base == "" {
		base = h.root
	}
	var candidate string
	switch {
	case strings.HasPrefix(p, "/"):
		candidate = path.Clean(p)
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
// the host itself uses, and remembers that name for the load that follows. Two
// imports that reach one file through different names resolve to the same
// module, so the bundler includes it once.
func (h *plugin) serves(candidate string) (string, bool) {
	name, err := rel(candidate, h.root)
	if err != nil {
		return "", false
	}
	if !h.files.FileExists(name) {
		return "", false
	}
	if real, err := h.files.Realpath(name); err == nil {
		canonical, err := rel(real, h.root)
		if err != nil {
			// The host is trusted for bytes, not for staying inside its own
			// project. Reading the name it replaced would hide the defect.
			h.fail(fmt.Errorf("compile: the host resolves %q to %q, which is not in the project", name, real))
			return "", false
		}
		name = canonical
	}
	full := h.root + "/" + name
	h.keep(full, name)
	return full, true
}

func artifact(resp map[string]any, root string) (Artifact, error) {
	var art Artifact
	for _, f := range list(resp["outputFiles"]) {
		f, ok := f.(map[string]any)
		if !ok {
			continue
		}
		p, _ := f["path"].(string)
		name, err := rel(p, root)
		if err != nil {
			return Artifact{}, fmt.Errorf("compile: esbuild wrote %q, which is not in the project", p)
		}
		bytes, _ := f["contents"].([]byte)
		art.Files = append(art.Files, File{Path: name, Bytes: bytes})
	}
	art.Diagnostics = append(art.Diagnostics, messages(resp["errors"], Error, root)...)
	art.Diagnostics = append(art.Diagnostics, messages(resp["warnings"], Warning, root)...)
	return art, nil
}

func list(v any) []any {
	a, _ := v.([]any)
	return a
}

// messages normalizes esbuild's shape. Its column is a 0-based byte offset into
// the line, so it is the one number that has to be translated: everything a
// caller sees from this package counts UTF-16 code units from 1.
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
			line, _ := loc["line"].(int)
			column, _ := loc["column"].(int)
			text, _ := loc["lineText"].(string)
			if name, err := rel(strings.TrimPrefix(file, space+":"), root); err == nil {
				d.File = name
			}
			d.Line = line
			d.Column = units(text, column)
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

// units turns esbuild's column into the one every other checker reports. esbuild
// counts bytes from 0 and a compiler counts UTF-16 code units from 1, so the
// same position in a line holding a multi-byte rune is a different number in
// each: on `import { nope as ééé } from "./gone";` the opening quote is 29 to
// tsgo and 32 to esbuild, and a caller that has to know which one answered does
// not have one diagnostic shape.
func units(line string, offset int) int {
	if offset < 0 {
		offset = 0
	}
	if offset > len(line) {
		// With no line there is nothing to count runes in, and esbuild's own
		// number is already right for a line that is all ASCII.
		return offset + 1
	}
	n := 1
	for _, r := range line[:offset] {
		n++
		if r > 0xFFFF {
			n++ // a rune outside the basic plane is a surrogate pair
		}
	}
	return n
}
