// Copyright © 2026 Hanzo AI. MIT License.

package wasm

import (
	"bytes"
	"errors"
)

// ErrMemory64Unsupported means a guest declares a 64-bit-indexed linear
// memory. The current sandbox backend is wazero, which does not yet support
// the Memory64 proposal. This is a backend limit, NOT a limit of Go's native
// process address space and NOT a limit of the separate wasm2go AOT compiler.
//
// AOT-generated Go is native code and DOES NOT preserve wazero isolation.
// Do not silently convert an untrusted sandbox request into native execution.
var ErrMemory64Unsupported = errors.New("wasm: Memory64 guest requires a Memory64-capable sandbox backend; use hanzoai/wasm2go for trusted native AOT code, not as a sandbox replacement")

// usesMemory64 examines only the memory declarations in the wasm binary:
// section 5 (defined memories) and section 2 (imported memories).
//
// This is a non-validating feature probe, not a second wasm parser. A corrupt,
// truncated or non-wasm binary is left to wazero's validation. It never reads
// outside input bounds or allocates based on untrusted lengths.
func usesMemory64(b []byte) bool {
	if len(b) < 8 || !bytes.Equal(b[:8], []byte{'\x00', 'a', 's', 'm', '\x01', 0, 0, 0}) {
		return false
	}
	pos := 8
	for pos < len(b) {
		id := b[pos]
		pos++
		n, ok := readULEB(b, &pos)
		if !ok || n > uint64(len(b)-pos) {
			return false
		}
		end := pos + int(n)
		section := b[pos:end]
		pos = end
		switch id {
		case 2:
			if importsMemory64(section) {
				return true
			}
		case 5:
			if memoriesMemory64(section) {
				return true
			}
		}
	}
	return false
}

func readULEB(b []byte, pos *int) (uint64, bool) {
	var n uint64
	for shift := uint(0); shift < 70; shift += 7 {
		if *pos >= len(b) {
			return 0, false
		}
		c := b[*pos]
		*pos++
		if shift == 63 && c > 1 {
			return 0, false
		}
		if shift > 63 {
			return 0, false
		}
		n |= uint64(c&0x7f) << shift
		if c&0x80 == 0 {
			return n, true
		}
	}
	return 0, false
}

func skipName(b []byte, p *int) bool {
	n, ok := readULEB(b, p)
	if !ok || n > uint64(len(b)-*p) {
		return false
	}
	*p += int(n)
	return true
}

// memoryLimits reads flags plus the LEB width appropriate to its memory.
// Table limits share the same layout but the table's table64 flag does not
// imply the guest has Memory64.
func memoryLimits(b []byte, p *int) (bool, bool) {
	flags, ok := readULEB(b, p)
	if !ok || flags > 7 {
		return false, false
	}
	min, ok := readULEB(b, p)
	_ = min
	if !ok {
		return false, false
	}
	if flags&1 != 0 {
		if _, ok := readULEB(b, p); !ok {
			return false, false
		}
	}
	return flags&4 != 0, true
}

func memoriesMemory64(b []byte) bool {
	p := 0
	n, ok := readULEB(b, &p)
	if !ok || n > uint64(len(b)) {
		return false
	}
	for i := uint64(0); i < n; i++ {
		is64, ok := memoryLimits(b, &p)
		if !ok {
			return false
		}
		if is64 {
			return true
		}
	}
	return false
}

func importsMemory64(b []byte) bool {
	p := 0
	n, ok := readULEB(b, &p)
	if !ok || n > uint64(len(b)) {
		return false
	}
	for i := uint64(0); i < n; i++ {
		if !skipName(b, &p) || !skipName(b, &p) || p >= len(b) {
			return false
		}
		kind := b[p]
		p++
		switch kind {
		case 0: // imported function type index
			if _, ok := readULEB(b, &p); !ok {
				return false
			}
		case 1: // table reftype and limits
			if p >= len(b) {
				return false
			}
			p++
			if _, ok := memoryLimits(b, &p); !ok {
				return false
			}
		case 2: // imported memory
			is64, ok := memoryLimits(b, &p)
			if !ok {
				return false
			}
			if is64 {
				return true
			}
		case 3: // global type + mutability
			if len(b)-p < 2 {
				return false
			}
			p += 2
		case 4: // exception tag attribute + type index
			if _, ok := readULEB(b, &p); !ok {
				return false
			}
			if _, ok := readULEB(b, &p); !ok {
				return false
			}
		default:
			return false
		}
	}
	return false
}
