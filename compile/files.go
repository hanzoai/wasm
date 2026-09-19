// Copyright © 2026 Hanzo AI. MIT License.

package compile

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"
)

// Map serves a project from memory, keyed by project-relative name. It is what
// a test hands a checker and the shape a host's local cache of an s3 tree takes
// once it is read: directories are implied by the keys, so there is nothing to
// keep consistent.
type Map map[string][]byte

var _ Files = Map(nil)

func clean(name string) string {
	name = path.Clean("/" + name)
	return strings.TrimPrefix(name, "/")
}

// ReadFile returns one file's bytes.
func (m Map) ReadFile(name string) ([]byte, error) {
	b, ok := m[clean(name)]
	if !ok {
		return nil, fmt.Errorf("compile: no file %q in project", name)
	}
	return b, nil
}

// FileExists reports whether name is a file in the map.
func (m Map) FileExists(name string) bool {
	_, ok := m[clean(name)]
	return ok
}

// DirExists reports whether any file lives under name.
func (m Map) DirExists(name string) bool {
	name = clean(name)
	if name == "" {
		return true
	}
	for k := range m {
		if strings.HasPrefix(k, name+"/") {
			return true
		}
	}
	return false
}

// Entries lists the bare names directly under a directory.
func (m Map) Entries(name string) ([]string, error) {
	dir := clean(name)
	if dir != "" && !m.DirExists(dir) {
		return nil, fmt.Errorf("compile: no directory %q in project", name)
	}
	seen := map[string]bool{}
	prefix := ""
	if dir != "" {
		prefix = dir + "/"
	}
	for k := range m {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		rest := k[len(prefix):]
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			rest = rest[:i]
		}
		if rest != "" {
			seen[rest] = true
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

// Realpath returns name cleaned; a map holds no aliases.
func (m Map) Realpath(name string) (string, error) {
	if name := clean(name); m.FileExists(name) || m.DirExists(name) {
		return name, nil
	}
	return "", fmt.Errorf("compile: no such name %q in project", name)
}

// tree adapts Files to fs.FS so a guest that issues ordinary WASI reads — tsgo
// does, and porting it away from a filesystem would mean forking a compiler —
// still reads nothing but what the host answers. There is no disk behind it and
// no preopened directory: wazero translates the guest's calls into these
// methods, and a name the host declines is a name the guest cannot reach.
//
// over holds files the host synthesized, and shadows the project. It is how a
// project with no tsconfig.json gets checked without one being written
// anywhere.
type tree struct {
	files Files
	over  map[string][]byte
}

var (
	_ fs.FS        = (*tree)(nil)
	_ fs.StatFS    = (*tree)(nil)
	_ fs.ReadDirFS = (*tree)(nil)
)

func (t *tree) read(name string) ([]byte, bool) {
	if b, ok := t.over[name]; ok {
		return b, true
	}
	// Ask the host what it calls this file before reading it: a host that
	// serves an aliased tree answers reads under the canonical name, and a
	// host with no aliases hands the same name back. The answer is held to the
	// same rule as the question — Open guards what the guest asked for, and an
	// alias out of the project would walk straight past that guard.
	if real, err := t.files.Realpath(name); err == nil {
		if !fs.ValidPath(real) {
			return nil, false
		}
		name = real
	}
	if !t.files.FileExists(name) {
		return nil, false
	}
	b, err := t.files.ReadFile(name)
	if err != nil {
		return nil, false
	}
	return b, true
}

func (t *tree) isDir(name string) bool {
	if name == "." {
		return true
	}
	if t.files.DirExists(name) {
		return true
	}
	for k := range t.over {
		if strings.HasPrefix(k, name+"/") {
			return true
		}
	}
	return false
}

func (t *tree) entries(name string) []string {
	seen := map[string]bool{}
	if list, err := t.files.Entries(name); err == nil {
		for _, n := range list {
			// A listing is bare names. One carrying a separator, or naming a
			// parent, is not an entry of this directory and would be joined
			// into a name that leaves it.
			if n == "." || n == ".." || strings.ContainsRune(n, '/') {
				continue
			}
			seen[n] = true
		}
	}
	prefix := ""
	if name != "." {
		prefix = name + "/"
	}
	for k := range t.over {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		rest := k[len(prefix):]
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			rest = rest[:i]
		}
		if rest != "" {
			seen[rest] = true
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Open serves one name. fs.FS names are already project-relative and
// unrooted, so a guest walking out of the project cannot name anything.
func (t *tree) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	if b, ok := t.read(name); ok {
		return &openFile{info: info{name: path.Base(name), size: int64(len(b))}, r: bytes.NewReader(b)}, nil
	}
	if t.isDir(name) {
		return &openDir{t: t, name: name}, nil
	}
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

// Stat answers without reading a file's bytes where it can.
func (t *tree) Stat(name string) (fs.FileInfo, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrInvalid}
	}
	if t.isDir(name) {
		return info{name: path.Base(name), dir: true}, nil
	}
	if b, ok := t.read(name); ok {
		return info{name: path.Base(name), size: int64(len(b))}, nil
	}
	return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
}

// ReadDir lists a directory.
func (t *tree) ReadDir(name string) ([]fs.DirEntry, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrInvalid}
	}
	if !t.isDir(name) {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrNotExist}
	}
	names := t.entries(name)
	out := make([]fs.DirEntry, 0, len(names))
	for _, n := range names {
		full := n
		if name != "." {
			full = name + "/" + n
		}
		if t.isDir(full) {
			out = append(out, fs.FileInfoToDirEntry(info{name: n, dir: true}))
			continue
		}
		b, _ := t.read(full)
		out = append(out, fs.FileInfoToDirEntry(info{name: n, size: int64(len(b))}))
	}
	return out, nil
}

type info struct {
	name string
	size int64
	dir  bool
}

func (i info) Name() string { return i.name }
func (i info) Size() int64  { return i.size }
func (i info) Mode() fs.FileMode {
	if i.dir {
		return fs.ModeDir | 0o555
	}
	return 0o444
}
func (i info) ModTime() time.Time { return time.Time{} }
func (i info) IsDir() bool        { return i.dir }
func (i info) Sys() any           { return nil }

type openFile struct {
	info info
	r    *bytes.Reader
}

func (f *openFile) Stat() (fs.FileInfo, error) { return f.info, nil }
func (f *openFile) Read(p []byte) (int, error) { return f.r.Read(p) }
func (f *openFile) Seek(off int64, whence int) (int64, error) {
	return f.r.Seek(off, whence)
}
func (f *openFile) Close() error { return nil }

type openDir struct {
	t    *tree
	name string
	at   int
}

func (d *openDir) Stat() (fs.FileInfo, error) {
	return info{name: path.Base(d.name), dir: true}, nil
}
func (d *openDir) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.name, Err: fs.ErrInvalid}
}
func (d *openDir) Close() error { return nil }

func (d *openDir) ReadDir(n int) ([]fs.DirEntry, error) {
	all, err := d.t.ReadDir(d.name)
	if err != nil {
		return nil, err
	}
	if d.at >= len(all) {
		if n <= 0 {
			return nil, nil
		}
		return nil, io.EOF
	}
	rest := all[d.at:]
	if n > 0 && n < len(rest) {
		rest = rest[:n]
	}
	d.at += len(rest)
	return rest, nil
}
