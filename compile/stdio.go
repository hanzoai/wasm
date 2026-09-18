// Copyright © 2026 Hanzo AI. MIT License.

package compile

// esbuild's stdio service protocol. Packets are length-prefixed, the id is
// shifted left with the low bit marking a response, and a value is a small
// tagged tree: nil, bool, int, string, bytes, array, map. This mirrors
// esbuild's own cmd/esbuild/stdio_protocol.go, which is the only definition
// there is — the protocol has no schema file and no version but the handshake.
//
// It is the reason a bundler needs no filesystem. The guest asks the host to
// resolve and load every import over this channel, so the module runs with zero
// preopened directories and can reach exactly the files the host answers for.

import (
	"encoding/binary"
	"fmt"
	"io"
	"sort"
	"sync"
)

func putU32(b []byte, v uint32) []byte {
	b = append(b, 0, 0, 0, 0)
	binary.LittleEndian.PutUint32(b[len(b)-4:], v)
	return b
}

func encode(id uint32, isRequest bool, value any) ([]byte, error) {
	var (
		out   []byte
		visit func(any) error
	)
	visit = func(value any) error {
		switch v := value.(type) {
		case nil:
			out = append(out, 0)
		case bool:
			n := uint8(0)
			if v {
				n = 1
			}
			out = append(out, 1, n)
		case int:
			out = append(out, 2)
			out = putU32(out, uint32(v))
		case string:
			out = append(out, 3)
			out = putU32(out, uint32(len(v)))
			out = append(out, v...)
		case []byte:
			out = append(out, 4)
			out = putU32(out, uint32(len(v)))
			out = append(out, v...)
		case []any:
			out = append(out, 5)
			out = putU32(out, uint32(len(v)))
			for _, item := range v {
				if err := visit(item); err != nil {
					return err
				}
			}
		case map[string]any:
			keys := make([]string, 0, len(v))
			for k := range v {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			out = append(out, 6)
			out = putU32(out, uint32(len(keys)))
			for _, k := range keys {
				out = putU32(out, uint32(len(k)))
				out = append(out, k...)
				if err := visit(v[k]); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("compile: cannot encode %T", value)
		}
		return nil
	}
	out = putU32(out, 0)
	if isRequest {
		out = putU32(out, id<<1)
	} else {
		out = putU32(out, (id<<1)|1)
	}
	if err := visit(value); err != nil {
		return nil, err
	}
	putU32(out[:0], uint32(len(out)-4))
	return out, nil
}

func decode(b []byte) (id uint32, isRequest bool, value any, err error) {
	defer func() {
		// A truncated or malformed packet indexes past the buffer. The guest is
		// the only writer and a malformed packet means the session is over, so
		// turn it into an error rather than a panic in the host.
		if r := recover(); r != nil {
			err = fmt.Errorf("compile: malformed packet")
		}
	}()
	u32 := func() uint32 {
		v := binary.LittleEndian.Uint32(b)
		b = b[4:]
		return v
	}
	var visit func() any
	visit = func() any {
		kind := b[0]
		b = b[1:]
		switch kind {
		case 0:
			return nil
		case 1:
			v := b[0] != 0
			b = b[1:]
			return v
		case 2:
			return int(u32())
		case 3:
			n := u32()
			s := string(b[:n])
			b = b[n:]
			return s
		case 4:
			n := u32()
			s := append([]byte{}, b[:n]...)
			b = b[n:]
			return s
		case 5:
			n := u32()
			a := make([]any, n)
			for i := range a {
				a[i] = visit()
			}
			return a
		case 6:
			n := u32()
			m := make(map[string]any, n)
			for i := uint32(0); i < n; i++ {
				kn := u32()
				k := string(b[:kn])
				b = b[kn:]
				m[k] = visit()
			}
			return m
		}
		panic(fmt.Sprintf("packet kind %d", kind))
	}
	raw := u32()
	return raw >> 1, raw&1 == 0, visit(), nil
}

// service is one side of the protocol: requests out, requests in, answers
// matched by id. Both directions are requests, which is what makes a plugin
// possible — the guest asks the host to resolve an import while the host is
// waiting for the build it asked for.
type service struct {
	w io.Writer
	r io.Reader

	mu      sync.Mutex
	next    uint32
	pending map[uint32]chan any

	// answer handles a request from the guest: on-start, on-resolve, on-load,
	// on-end, ping.
	answer func(map[string]any) map[string]any

	version string
	fail    chan error
	once    sync.Once
}

func newService(w io.Writer, r io.Reader) *service {
	return &service{w: w, r: r, pending: map[uint32]chan any{}, fail: make(chan error, 1)}
}

// handshake reads the bare length-prefixed version string the guest emits
// first, before any packet.
func (s *service) handshake() error {
	var hdr [4]byte
	if _, err := io.ReadFull(s.r, hdr[:]); err != nil {
		return fmt.Errorf("compile: esbuild version header: %w", err)
	}
	buf := make([]byte, binary.LittleEndian.Uint32(hdr[:]))
	if _, err := io.ReadFull(s.r, buf); err != nil {
		return fmt.Errorf("compile: esbuild version: %w", err)
	}
	s.version = string(buf)
	return nil
}

func (s *service) stop(err error) {
	s.once.Do(func() { s.fail <- err })
}

// serve reads until the stream ends, answering the guest's requests and
// delivering responses to whoever is waiting for them.
func (s *service) serve() {
	var hdr [4]byte
	for {
		if _, err := io.ReadFull(s.r, hdr[:]); err != nil {
			s.stop(err)
			return
		}
		buf := make([]byte, binary.LittleEndian.Uint32(hdr[:]))
		if _, err := io.ReadFull(s.r, buf); err != nil {
			s.stop(err)
			return
		}
		id, isRequest, value, err := decode(buf)
		if err != nil {
			s.stop(err)
			return
		}
		if isRequest {
			req, _ := value.(map[string]any)
			var resp map[string]any
			if s.answer != nil {
				resp = s.answer(req)
			}
			if resp == nil {
				resp = map[string]any{}
			}
			packet, err := encode(id, false, resp)
			if err != nil {
				s.stop(err)
				return
			}
			s.mu.Lock()
			_, err = s.w.Write(packet)
			s.mu.Unlock()
			if err != nil {
				s.stop(err)
				return
			}
			continue
		}
		s.mu.Lock()
		ch := s.pending[id]
		delete(s.pending, id)
		s.mu.Unlock()
		if ch != nil {
			ch <- value
		}
	}
}

// send issues one request and waits for its answer, or for the stream to end.
func (s *service) send(req map[string]any) (map[string]any, error) {
	ch := make(chan any, 1)
	s.mu.Lock()
	id := s.next
	s.next++
	s.pending[id] = ch
	packet, err := encode(id, true, req)
	if err == nil {
		_, err = s.w.Write(packet)
	}
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	select {
	case v := <-ch:
		m, _ := v.(map[string]any)
		return m, nil
	case err := <-s.fail:
		return nil, fmt.Errorf("compile: esbuild stream ended: %w", err)
	}
}
