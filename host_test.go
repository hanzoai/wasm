package wasm_test

import (
	"bytes"
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/hanzoai/wasm"
)

// mem is a Store in a map — the point of the interface is that a guest cannot
// tell this from hanzoai/s3 over ZAP.
type mem struct {
	mu sync.Mutex
	m  map[string][]byte
}

func newMem() *mem { return &mem{m: map[string][]byte{}} }

func (s *mem) Get(_ context.Context, k string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[k]
	if !ok {
		return nil, nil
	}
	return v, nil
}

func (s *mem) Put(_ context.Context, k string, v []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[k] = v
	return nil
}

func (s *mem) List(_ context.Context, pfx string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k := range s.m {
		if strings.HasPrefix(k, pfx) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Binding must not require a guest to exist yet: a host binds once at startup
// and instantiates many guests afterwards.
func TestBindBeforeAnyGuest(t *testing.T) {
	ctx := context.Background()
	e, err := wasm.New(ctx, wasm.Limits{})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer e.Close(ctx)
	if err := e.Bind(ctx, newMem()); err != nil {
		t.Fatalf("bind: %v", err)
	}
}

// A module that imports the host module must instantiate against it. This is the
// contract that makes S3-backed guests possible at all: the import resolves, so
// a guest can be compiled against `hanzo` without the host being present at
// compile time.
func TestGuestImportsResolve(t *testing.T) {
	ctx := context.Background()
	e, err := wasm.New(ctx, wasm.Limits{})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer e.Close(ctx)
	if err := e.Bind(ctx, newMem()); err != nil {
		t.Fatalf("bind: %v", err)
	}
	// (module (import "hanzo" "get" (func (param i32 i32 i32) (result i32))) (memory 1))
	src := []byte{
		0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
		0x01, 0x08, 0x01, 0x60, 0x03, 0x7f, 0x7f, 0x7f, 0x01, 0x7f,
		0x02, 0x0d, 0x01, 0x05, 0x68, 0x61, 0x6e, 0x7a, 0x6f, 0x03, 0x67, 0x65, 0x74, 0x00, 0x00,
		0x05, 0x03, 0x01, 0x00, 0x01,
	}
	m, err := e.Compile(ctx, src)
	if err != nil {
		t.Fatalf("compile a module importing hanzo.get: %v", err)
	}
	in, err := m.Start(ctx)
	if err != nil {
		t.Fatalf("the host import did not resolve: %v", err)
	}
	_ = in.Close(ctx)
}

// A guest that imports hanzo.get without a Bind must FAIL to start, and say so.
// Silently starting one that cannot reach its store is the failure worth
// preventing: it would look sandboxed and do nothing.
func TestUnboundHostIsRefused(t *testing.T) {
	ctx := context.Background()
	e, err := wasm.New(ctx, wasm.Limits{})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer e.Close(ctx)
	src := []byte{
		0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
		0x01, 0x08, 0x01, 0x60, 0x03, 0x7f, 0x7f, 0x7f, 0x01, 0x7f,
		0x02, 0x0d, 0x01, 0x05, 0x68, 0x61, 0x6e, 0x7a, 0x6f, 0x03, 0x67, 0x65, 0x74, 0x00, 0x00,
		0x05, 0x03, 0x01, 0x00, 0x01,
	}
	m, err := e.Compile(ctx, src)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if _, err := m.Start(ctx); err == nil {
		t.Fatal("a guest reached an unbound host module without complaint")
	}
}

// Put must copy. The slice a host function reads aliases guest memory, and a
// store that kept it would persist whatever the guest wrote next.
func TestStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newMem()
	if err := s.Put(ctx, "repo/HEAD", []byte("ref: refs/heads/main")); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := s.Get(ctx, "repo/HEAD")
	if err != nil || string(got) != "ref: refs/heads/main" {
		t.Fatalf("get = %q, %v", got, err)
	}
	keys, err := s.List(ctx, "repo/")
	if err != nil || len(keys) != 1 || keys[0] != "repo/HEAD" {
		t.Fatalf("list = %v, %v", keys, err)
	}
	if missing, _ := s.Get(ctx, "nope"); missing != nil {
		t.Fatal("absence must read as nil, not as an error")
	}
}

// storeGuest hands each call to the host unchanged, so a test sees exactly what
// the host answered, and allocates by bumping a pointer up from 1024.
//
//	(import "hanzo" "get"  (func (param i32 i32 i32) (result i32)))
//	(import "hanzo" "list" (func (param i32 i32 i32) (result i32)))
//	(import "hanzo" "put"  (func (param i32 i32 i32 i32) (result i32)))
//	(memory (export "memory") 1)
//	(export "get" "list" "put")  ;; each calls its import with its own params
//	(export "alloc" (func (param i32) (result i32)))
var storeGuest = []byte{
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00, 0x01, 0x15, 0x03, 0x60,
	0x03, 0x7f, 0x7f, 0x7f, 0x01, 0x7f, 0x60, 0x04, 0x7f, 0x7f, 0x7f, 0x7f,
	0x01, 0x7f, 0x60, 0x01, 0x7f, 0x01, 0x7f, 0x02, 0x26, 0x03, 0x05, 0x68,
	0x61, 0x6e, 0x7a, 0x6f, 0x03, 0x67, 0x65, 0x74, 0x00, 0x00, 0x05, 0x68,
	0x61, 0x6e, 0x7a, 0x6f, 0x04, 0x6c, 0x69, 0x73, 0x74, 0x00, 0x00, 0x05,
	0x68, 0x61, 0x6e, 0x7a, 0x6f, 0x03, 0x70, 0x75, 0x74, 0x00, 0x01, 0x03,
	0x05, 0x04, 0x00, 0x00, 0x01, 0x02, 0x05, 0x03, 0x01, 0x00, 0x01, 0x06,
	0x07, 0x01, 0x7f, 0x01, 0x41, 0x80, 0x08, 0x0b, 0x07, 0x25, 0x05, 0x06,
	0x6d, 0x65, 0x6d, 0x6f, 0x72, 0x79, 0x02, 0x00, 0x03, 0x67, 0x65, 0x74,
	0x00, 0x03, 0x04, 0x6c, 0x69, 0x73, 0x74, 0x00, 0x04, 0x03, 0x70, 0x75,
	0x74, 0x00, 0x05, 0x05, 0x61, 0x6c, 0x6c, 0x6f, 0x63, 0x00, 0x06, 0x0a,
	0x30, 0x04, 0x0a, 0x00, 0x20, 0x00, 0x20, 0x01, 0x20, 0x02, 0x10, 0x00,
	0x0b, 0x0a, 0x00, 0x20, 0x00, 0x20, 0x01, 0x20, 0x02, 0x10, 0x01, 0x0b,
	0x0c, 0x00, 0x20, 0x00, 0x20, 0x01, 0x20, 0x02, 0x20, 0x03, 0x10, 0x02,
	0x0b, 0x0b, 0x00, 0x23, 0x00, 0x23, 0x00, 0x20, 0x00, 0x6a, 0x24, 0x00,
	0x0b,
}

// broken is a Store whose every call fails, as s3 does when the network under
// it does.
type broken struct{}

var errBroken = errors.New("store unreachable")

func (broken) Get(context.Context, string) ([]byte, error)    { return nil, errBroken }
func (broken) Put(context.Context, string, []byte) error      { return errBroken }
func (broken) List(context.Context, string) ([]string, error) { return nil, errBroken }

// guest starts src with s bound to it.
func guest(t *testing.T, s wasm.Store, src []byte) *wasm.Instance {
	t.Helper()
	ctx := context.Background()
	e, err := wasm.New(ctx, wasm.Limits{})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	t.Cleanup(func() { _ = e.Close(ctx) })
	if err := e.Bind(ctx, s); err != nil {
		t.Fatalf("bind: %v", err)
	}
	m, err := e.Compile(ctx, src)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	in, err := m.Start(ctx)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	return in
}

// outPtr is where a guest asks for a result's address: clear of the key at 0
// and of the allocator, which starts at 1024.
const outPtr = 512

// ask has the guest call fn, get or list, with key, and returns the host's
// answer and, when the answer is a length, the bytes it handed over.
func ask(t *testing.T, in *wasm.Instance, fn, key string) (int32, string, error) {
	t.Helper()
	if !in.Memory().Write(0, []byte(key)) {
		t.Fatalf("write key %q", key)
	}
	out, err := in.Call(context.Background(), fn, 0, uint64(len(key)), outPtr)
	if err != nil {
		return 0, "", err
	}
	n := int32(out[0])
	if n < 0 {
		return n, "", nil
	}
	ptr, _ := in.Memory().ReadUint32Le(outPtr)
	val, _ := in.Memory().Read(ptr, uint32(n))
	return n, string(val), nil
}

// Every answer a guest can get, and none may stand in for another. Above all
// "absent" and "the store failed": a guest walking a repository that reads a
// failed fetch as a missing object has been told something false.
func TestStoreAnswers(t *testing.T) {
	ctx := context.Background()
	s := newMem()
	_ = s.Put(ctx, "repo/HEAD", []byte("ref: refs/heads/main"))
	_ = s.Put(ctx, "repo/config", []byte("[core]"))
	in := guest(t, s, storeGuest)
	down := guest(t, broken{}, storeGuest)

	for _, c := range []struct {
		in       *wasm.Instance
		fn, key  string
		n        int32
		val, why string
	}{
		{in, "get", "repo/HEAD", 20, "ref: refs/heads/main", "a present key"},
		{in, "get", "repo/nope", -1, "", "an absent key"},
		{down, "get", "repo/HEAD", -2, "", "a failed store"},
		{in, "list", "repo/", 21, "repo/HEAD\nrepo/config", "a prefix"},
		{down, "list", "repo/", -1, "", "a failed store"},
	} {
		n, val, err := ask(t, c.in, c.fn, c.key)
		if err != nil || n != c.n || val != c.val {
			t.Errorf("%s on %s = %d %q %v, want %d %q", c.fn, c.why, n, val, err, c.n, c.val)
		}
	}

	// put answers 0 or, when the store failed, -1. And it stores a COPY: the
	// guest rewriting its buffer afterwards must not reach the stored value.
	in.Memory().Write(0, []byte("k"))
	in.Memory().Write(16, []byte("value"))
	if out, err := in.Call(ctx, "put", 0, 1, 16, 5); err != nil || int32(out[0]) != 0 {
		t.Fatalf("put = %v %v, want 0", out, err)
	}
	in.Memory().Write(16, []byte("XXXXX"))
	if v, _ := s.Get(ctx, "k"); string(v) != "value" {
		t.Fatalf("stored %q, want %q", v, "value")
	}
	down.Memory().Write(0, []byte("k"))
	if out, err := down.Call(ctx, "put", 0, 1, 16, 5); err != nil || int32(out[0]) != -1 {
		t.Fatalf("put on a failed store = %v %v, want -1", out, err)
	}
}

// A guest with no alloc(i32) i32 has nowhere to receive a value. It traps with
// ErrNoAlloc; handed -1 instead, it would read a key that exists as absent.
// Absence hands nothing over, so it needs no allocator.
func TestNoAllocIsNamed(t *testing.T) {
	ctx := context.Background()
	s := newMem()
	_ = s.Put(ctx, "repo/HEAD", []byte("ref: refs/heads/main"))
	for why, src := range map[string][]byte{
		// The same guest with its allocator exported under another name,
		"no alloc": bytes.Replace(storeGuest, []byte("alloc"), []byte("other"), 1),
		// and with "alloc" naming function 5, the put that takes four params.
		"alloc of the wrong type": bytes.Replace(storeGuest, []byte("alloc\x00\x06"), []byte("alloc\x00\x05"), 1),
	} {
		in := guest(t, s, src)
		for _, fn := range []string{"get", "list"} {
			if n, _, err := ask(t, in, fn, "repo/HEAD"); !errors.Is(err, wasm.ErrNoAlloc) {
				t.Errorf("%s with %s = %d %v, want ErrNoAlloc", fn, why, n, err)
			}
		}
		if n, _, err := ask(t, in, "get", "repo/nope"); err != nil || n != -1 {
			t.Errorf("an absent key with %s = %d %v, want -1", why, n, err)
		}
	}
}

// An address outside the guest's memory is a bug in the guest, and traps as the
// guest's own out-of-bounds load would. Answered with -1, a key the host could
// not read would come back as a key that does not exist.
func TestBadAddressTraps(t *testing.T) {
	ctx := context.Background()
	s := newMem()
	_ = s.Put(ctx, "k", []byte("v"))
	in := guest(t, s, storeGuest)
	in.Memory().Write(0, []byte("k"))
	const past = 1 << 16 // the guest has one page, so this is the first address it lacks
	for _, c := range []struct {
		fn   string
		args []uint64
	}{
		{"get", []uint64{past, 1, outPtr}},
		{"get", []uint64{0, 1, past}},
		{"list", []uint64{past, 1, outPtr}},
		{"list", []uint64{0, 1, past}},
		{"put", []uint64{past, 1, 0, 1}},
		{"put", []uint64{0, 1, past, 1}},
	} {
		if out, err := in.Call(ctx, c.fn, c.args...); err == nil {
			t.Errorf("%s%v answered %d instead of trapping", c.fn, c.args, int32(out[0]))
		}
	}
}
