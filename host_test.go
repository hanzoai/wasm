package wasm_test

import (
	"context"
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
