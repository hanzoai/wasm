// Copyright © 2026 Hanzo AI. MIT License.

package wasm

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// Store is what a guest is allowed to reach. It is the WHOLE of the outside
// world from inside the sandbox.
//
// This is the answer to the objection that wasm cannot do real work because WASI
// has no process spawn. A guest that shells out to `git` needs exec and cannot
// run here. A guest handed an object store does not: the repository is objects,
// a read is Get, a write is Put, and the network hop happens on the HOST side
// where the credential already lives. Nothing is spawned because nothing needs
// to be.
//
// The implementation is the caller's — hanzoai/s3 over ZAP in the fleet, a map
// in a test. The guest cannot tell, which is the point: it names keys, not
// hosts, and holds no credential it could leak.
type Store interface {
	Get(ctx context.Context, key string) ([]byte, error)
	Put(ctx context.Context, key string, val []byte) error
	List(ctx context.Context, prefix string) ([]string, error)
}

// ErrNoAlloc means the guest did not export the allocator the host needs to
// hand it a result. It is a contract error, not a runtime one: a module that
// wants to receive bytes has to provide somewhere to put them. The guest traps
// with it, and the Call that asked returns it.
var ErrNoAlloc = errors.New("wasm: guest exports no alloc(i32) i32")

// The module a guest imports to reach its Store. One name, because a guest that
// had to know which host it was running under would be a guest that could be
// pointed at the wrong one.
const hostModule = "hanzo"

// Bind exposes a Store to every module this Engine instantiates afterwards.
//
// The wire is deliberately small: a key goes in as (ptr,len) read out of guest
// memory, and a result comes back by asking the guest to allocate and writing
// into that. No JSON crosses the boundary — it is bytes and two integers, which
// is the cheapest thing that can cross and the only shape that cannot disagree
// about a schema.
//
// A number answers the guest's question. A guest that breaks the contract gets
// no number: an address outside its memory, or a value it asked for with no
// alloc(i32) i32 to receive it (ErrNoAlloc), traps, and the Call that led there
// returns why. Answered with -1 instead, a guest bug would read as "absent".
func (e *Engine) Bind(ctx context.Context, s Store) error {
	b := e.rt.NewHostModuleBuilder(hostModule)

	// get(keyPtr, keyLen, outPtrPtr) -> length, with the value's address written
	// to outPtrPtr; -1 when the key is absent; -2 when the Store failed, which
	// says nothing about whether the key exists.
	//
	// Absence is a VALUE and not a trap: a guest asking whether a file exists is
	// asking a question, and a trap would make the answer unrecoverable. A failed
	// Store is a value too, since the guest may ask again, but never the same
	// value: "no such object" is an answer and "could not fetch it" is not.
	b.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module, keyPtr, keyLen, outPtrPtr uint32) int32 {
		key := readString(m, keyPtr, keyLen)
		val, err := s.Get(ctx, key)
		if err != nil {
			return -2
		}
		if val == nil {
			return -1
		}
		ptr := give(ctx, m, val)
		write(m, outPtrPtr, ptr)
		return int32(len(val))
	}).Export("get")

	// put(keyPtr, keyLen, valPtr, valLen) -> 0 ok, -1 when the Store failed.
	b.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module, keyPtr, keyLen, valPtr, valLen uint32) int32 {
		key := readString(m, keyPtr, keyLen)
		val := read(m, valPtr, valLen)
		// COPY before handing it on. The slice above aliases guest memory, which
		// the guest may rewrite the moment this returns — a store that kept the
		// slice would persist whatever the guest wrote next.
		cp := make([]byte, len(val))
		copy(cp, val)
		if err := s.Put(ctx, key, cp); err != nil {
			return -1
		}
		return 0
	}).Export("put")

	// list(prefixPtr, prefixLen, outPtrPtr) -> length of a \n-joined list, with
	// its address written to outPtrPtr; -1 when the Store failed.
	b.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module, pfxPtr, pfxLen, outPtrPtr uint32) int32 {
		pfx := readString(m, pfxPtr, pfxLen)
		keys, err := s.List(ctx, pfx)
		if err != nil {
			return -1
		}
		var flat []byte
		for i, k := range keys {
			if i > 0 {
				flat = append(flat, '\n')
			}
			flat = append(flat, k...)
		}
		ptr := give(ctx, m, flat)
		write(m, outPtrPtr, ptr)
		return int32(len(flat))
	}).Export("list")

	if _, err := b.Instantiate(ctx); err != nil {
		return fmt.Errorf("wasm: bind %s: %w", hostModule, err)
	}
	return nil
}

// read returns n bytes of guest memory at ptr, aliasing it. An address outside
// that memory traps, as the guest's own out-of-bounds load would.
//
// A panic in a host function IS a trap: wazero unwinds the guest and returns the
// panic value, still wrapped, as the error of the Call that was running. That is
// how read, write and give refuse, and how ErrNoAlloc reaches errors.Is.
func read(m api.Module, ptr, n uint32) []byte {
	b, ok := m.Memory().Read(ptr, n)
	if !ok {
		panic(fmt.Errorf("wasm: %d bytes at %d are outside guest memory", n, ptr))
	}
	return b
}

// readString reads a guest string without keeping the guest's memory alive.
func readString(m api.Module, ptr, n uint32) string {
	return string(read(m, ptr, n)) // string() copies
}

// write stores a result's address where the guest asked for it.
func write(m api.Module, at, ptr uint32) {
	if !m.Memory().WriteUint32Le(at, ptr) {
		panic(fmt.Errorf("wasm: result address %d is outside guest memory", at))
	}
}

// give asks the guest for space and writes val into it. The guest owns the
// result afterwards, including freeing it — the host cannot know when a guest is
// done with a buffer, so it does not pretend to.
func give(ctx context.Context, m api.Module, val []byte) uint32 {
	alloc := m.ExportedFunction("alloc")
	if alloc == nil || !isAlloc(alloc.Definition()) {
		panic(ErrNoAlloc)
	}
	res, err := alloc.Call(ctx, uint64(len(val)))
	if err != nil {
		panic(fmt.Errorf("wasm: alloc(%d): %w", len(val), err))
	}
	ptr := uint32(res[0])
	if !m.Memory().Write(ptr, val) {
		panic(fmt.Errorf("wasm: alloc(%d) returned unwritable memory", len(val)))
	}
	return ptr
}

// isAlloc reports whether d is alloc(i32) i32, the one signature give can call.
func isAlloc(d api.FunctionDefinition) bool {
	i32 := []api.ValueType{api.ValueTypeI32}
	return slices.Equal(d.ParamTypes(), i32) && slices.Equal(d.ResultTypes(), i32)
}

// compile-time proof the builder type is what we think it is.
var _ = wazero.HostModuleBuilder(nil)
