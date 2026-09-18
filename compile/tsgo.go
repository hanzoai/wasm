// Copyright © 2026 Hanzo AI. MIT License.

package compile

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/sys"
)

// TSGo typechecks TypeScript. The module is upstream typescript-go built for
// wasip1: a command module, so one check is one instance, and the standard
// library is parsed again each time. That is the cost this type manages and the
// reason a reactor is the next step rather than a nicety.
//
// It is a Checker and not a Bundler. It emits nothing.
type TSGo struct {
	e *engine
}

var _ Checker = (*TSGo)(nil)

// NewTSGo compiles the module once. Compiling tsgo takes ~12s cold and ~300ms
// from the on-disk cache, so this belongs in a host's startup and not in a
// request.
func NewTSGo(ctx context.Context, c Config) (*TSGo, error) {
	e, err := newEngine(ctx, c)
	if err != nil {
		return nil, err
	}
	return &TSGo{e: e}, nil
}

// Close releases the runtime and the compiled module.
func (t *TSGo) Close(ctx context.Context) error { return t.e.Close(ctx) }

// Name is the token an agent asks for.
func (t *TSGo) Name() string { return "tsgo" }

const tsconfigName = "tsconfig.json"

var tsExt = map[string]bool{".ts": true, ".tsx": true, ".mts": true, ".cts": true}

// Detects reports a TypeScript project: a tsconfig.json, or a TypeScript entry.
// A project with neither is not guessed at.
func (t *TSGo) Detects(p Project) bool {
	if p.Files == nil {
		return false
	}
	if p.Files.FileExists(tsconfigName) {
		return true
	}
	for _, e := range p.Entry {
		if tsExt[path.Ext(e)] {
			return true
		}
	}
	return false
}

// Check runs one instance and returns normalized diagnostics.
//
// Precedence on libs and types is explicit-caller, then project, then host
// default: Options on the command line override a tsconfig the way tsc's own
// flags do, a project that declares its own is honored, and a project with no
// tsconfig at all gets es2022 with no ambient types — which is 273ms and 430MiB
// against 1.7s and 795MiB for the default DOM+ES bundle on the same two files.
func (t *TSGo) Check(ctx context.Context, p Project, o Options) ([]Diagnostic, error) {
	if p.Files == nil {
		return nil, fmt.Errorf("compile: tsgo: project serves no files")
	}
	limit, err := t.e.gomemlimit(o.Memory)
	if err != nil {
		return nil, err
	}
	root := strings.TrimSuffix(p.root(), "/")

	tree := &tree{files: p.Files}
	if !p.Files.FileExists(tsconfigName) {
		// With neither a config nor an entry there is nothing to name in a
		// synthesized one, and tsc's answer — "no inputs were found" — would
		// arrive as a diagnostic about source that does not exist.
		if names, err := p.Files.Entries("."); len(p.Entry) == 0 && (err != nil || len(names) == 0) {
			return nil, fmt.Errorf("compile: tsgo: project has no %s and no files", tsconfigName)
		}
		cfg, err := synthesize(p, o)
		if err != nil {
			return nil, err
		}
		tree.over = map[string][]byte{tsconfigName: cfg}
	}

	args := []string{"tsgo", "--pretty", "false", "--noEmit", "--project", root + "/" + tsconfigName}
	if len(o.Lib) > 0 {
		args = append(args, "--lib", strings.Join(o.Lib, ","))
	}
	if len(o.Types) > 0 {
		args = append(args, "--types", strings.Join(o.Types, ","))
	}

	var out, errb bytes.Buffer
	cfg := wazero.NewModuleConfig().
		WithName(t.e.name("tsgo")).
		WithArgs(args...).
		WithStdout(&out).WithStderr(&errb).
		WithEnv("GOMEMLIMIT", limit).
		WithFSConfig(wazero.NewFSConfig().WithFSMount(tree, root)).
		WithSysWalltime().WithSysNanotime().WithSysNanosleep()

	code := 0
	mod, err := t.e.rt.InstantiateModule(ctx, t.e.mod, cfg)
	if mod != nil {
		_ = mod.Close(ctx)
	}
	if err != nil {
		if ctx.Err() != nil {
			// A cancelled context closes the instance and surfaces as an exit
			// code; report what actually happened.
			return nil, ctx.Err()
		}
		exit, ok := err.(*sys.ExitError)
		if !ok {
			return nil, fmt.Errorf("compile: tsgo: %w", err)
		}
		code = int(exit.ExitCode())
	}

	diags := parseTSC(out.String(), root)
	if len(diags) == 0 && code != 0 {
		return nil, fmt.Errorf("compile: tsgo exited %d: %s", code, tail(out.String(), errb.String()))
	}
	return diags, nil
}

// synthesize writes the tsconfig a project without one would have written. It
// exists in the guest's view of the project and nowhere else — no file is
// created on any disk, and the project the caller handed us is unchanged.
func synthesize(p Project, o Options) ([]byte, error) {
	lib := o.Lib
	if len(lib) == 0 {
		lib = []string{"es2022"}
	}
	types := o.Types
	if types == nil {
		types = []string{}
	}
	opts := map[string]any{
		"strict":           true,
		"noEmit":           true,
		"target":           "es2022",
		"module":           "esnext",
		"moduleResolution": "bundler",
		"lib":              lib,
		"types":            types,
	}
	cfg := map[string]any{"compilerOptions": opts}
	if len(p.Entry) > 0 {
		cfg["files"] = p.Entry
	} else {
		cfg["include"] = []string{"**/*"}
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("compile: tsgo: synthesize %s: %w", tsconfigName, err)
	}
	return b, nil
}

// tsc's plain output: one diagnostic per line, with a location or without.
// Related information and message chains arrive as following lines that match
// neither, and belong to the diagnostic above them.
var (
	located = regexp.MustCompile(`^(\S.*)\((\d+),(\d+)\): (error|warning|message) ([^:]+): (.*)$`)
	bare    = regexp.MustCompile(`^(error|warning|message) ([^:]+): (.*)$`)
)

func parseTSC(out, root string) []Diagnostic {
	var diags []Diagnostic
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if m := located.FindStringSubmatch(line); m != nil {
			l, _ := strconv.Atoi(m[2])
			c, _ := strconv.Atoi(m[3])
			diags = append(diags, Diagnostic{
				File:     rel(m[1], root),
				Line:     l,
				Column:   c,
				Severity: severity(m[4]),
				Code:     m[5],
				Message:  m[6],
				Checker:  "tsgo",
			})
			continue
		}
		if m := bare.FindStringSubmatch(line); m != nil {
			diags = append(diags, Diagnostic{
				Severity: severity(m[1]),
				Code:     m[2],
				Message:  m[3],
				Checker:  "tsgo",
			})
			continue
		}
		if n := len(diags); n > 0 {
			diags[n-1].Message += " " + strings.TrimSpace(line)
		}
	}
	return diags
}

func severity(s string) string {
	switch s {
	case "error":
		return Error
	case "warning":
		return Warning
	default:
		return Info
	}
}

// rel turns a name a guest printed into a project-relative one. tsc prints
// relative to the tsconfig's directory, which is the root, and esbuild prints
// the absolute name the host resolver returned — and a guest whose working
// directory is / prints the root itself as a leading segment. Trim the root in
// whichever of the three forms it arrives in, and leave a name outside the
// project alone.
func rel(name, root string) string {
	name = strings.TrimPrefix(name, "./")
	root = strings.TrimSuffix(root, "/")
	for _, prefix := range []string{root + "/", strings.TrimPrefix(root, "/") + "/"} {
		if prefix == "/" {
			continue // a root of "" or "/" has nothing to trim
		}
		if trimmed := strings.TrimPrefix(name, prefix); trimmed != name {
			return trimmed
		}
	}
	return strings.TrimPrefix(name, "/")
}

func tail(out, err string) string {
	s := strings.TrimSpace(out + "\n" + err)
	if len(s) > 600 {
		s = s[len(s)-600:]
	}
	if s == "" {
		return "no output"
	}
	return s
}
