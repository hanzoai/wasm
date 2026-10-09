package wasm

import (
	"context"
	"errors"
	"testing"
)

func wasmHeader() []byte {
	return []byte{0, 'a', 's', 'm', 1, 0, 0, 0}
}

func TestMemory64DefinedOrImported(t *testing.T) {
	// section 5: one memory, memory64 flag (0x04), minimum one page
	defined64 := append(wasmHeader(), []byte{5, 3, 1, 4, 1}...)
	// section 2: import "e"."m", kind memory (2), memory64 flag, min=1
	imported64 := append(wasmHeader(), []byte{2, 8, 1, 1, 'e', 1, 'm', 2, 4, 1}...)
	// section 5: ordinary wasm32 memory with min=1
	defined32 := append(wasmHeader(), []byte{5, 3, 1, 0, 1}...)
	// section 2: imported wasm32 memory
	imported32 := append(wasmHeader(), []byte{2, 8, 1, 1, 'e', 1, 'm', 2, 0, 1}...)
	// malformed section bounds must not crash the probe
	truncated := append(wasmHeader(), []byte{5, 9, 1}...)

	tests := []struct {
		name string
		code []byte
		is64 bool
	}{
		{"defined64", defined64, true},
		{"imported64", imported64, true},
		{"defined32", defined32, false},
		{"imported32", imported32, false},
		{"truncated", truncated, false},
		{"notWasm", []byte("not a wasm binary"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := usesMemory64(tc.code); got != tc.is64 {
				t.Fatalf("usesMemory64 = %v, want %v", got, tc.is64)
			}
			ctx := context.Background()
			e, err := New(ctx, Limits{NoWASI: true})
			if err != nil { t.Fatal(err) }
			defer e.Close(ctx)
			_, err = e.Compile(ctx, tc.code)
			if tc.is64 && !errors.Is(err, ErrMemory64Unsupported) {
				t.Fatalf("Memory64 must fail with named error, got %v", err)
			}
			if !tc.is64 && errors.Is(err, ErrMemory64Unsupported) {
				t.Fatalf("wrongly detected Memory64, got %v", err)
			}
		})
	}
}

func TestMemory64ImportAfterOtherImport(t *testing.T) {
	// Import count 2: first function "e"."f", type index 0;
	// then memory "e"."m", memory64 minimum 1.
	imports := []byte{2, 1, 'e', 1, 'f', 0, 0, 1, 'e', 1, 'm', 2, 4, 1}
	code := append(wasmHeader(), append([]byte{2, byte(len(imports))}, imports...)...)
	if !usesMemory64(code) {
		t.Fatal("missed Memory64 import following a function import")
	}
}

func FuzzMemory64ProbeDoesNotPanic(f *testing.F) {
	f.Add([]byte{0, 'a', 's', 'm', 1, 0, 0, 0, 5, 3, 1, 4, 1})
	f.Add([]byte{0, 'a', 's', 'm', 1, 0, 0, 0, 2, 8, 1, 1, 'e', 1, 'm', 2, 4, 1})
	f.Add([]byte{5, 255, 255, 255})
	f.Fuzz(func(t *testing.T, b []byte) { _ = usesMemory64(b) })
}
