// Copyright © 2026 Hanzo AI. MIT License.

package compile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"runtime"
	"sync"
	"testing"
)

// The numbers that matter are three: what a host pays at startup, what a check
// costs once warm, and what a fleet of them costs at once. Everything this
// package decides — compile once, instantiate many, minimal lib, a GC ceiling —
// shows up in one of the three.

func benchBlob(b *testing.B, env, sumEnv string) Blob {
	b.Helper()
	path := os.Getenv(env)
	if path == "" {
		b.Fatalf("%s is unset: point it at the wasm module. There is no number to report without it", env)
	}
	sum := os.Getenv(sumEnv)
	if sum == "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			b.Fatalf("read %s: %v", path, err)
		}
		h := sha256.Sum256(raw)
		sum = hex.EncodeToString(h[:])
	}
	return Blob{Path: path, Sum: sum}
}

// warm fills the compilation cache so a benchmark measures reading machine code
// back rather than producing it once. The ~12s compile is a different number,
// and one of them in a mean hides the other.
func warm(b *testing.B, blob Blob) {
	b.Helper()
	e, err := newEngine(context.Background(), Config{Blob: blob, Cache: cacheDir})
	if err != nil {
		b.Fatal(err)
	}
	e.Close(context.Background())
}

// Cold is a host starting up: read 49 MiB of machine code back out of the
// on-disk cache, then check. Without the cache this is the ~12s compile
// instead, which is the whole reason Config.Cache exists.
func BenchmarkTSGoCold(b *testing.B) {
	ctx := context.Background()
	blob := benchBlob(b, envTSGo, envTSGoSum)
	warm(b, blob)
	for b.Loop() {
		c, err := NewTSGo(ctx, Config{Blob: blob, Cache: cacheDir})
		if err != nil {
			b.Fatal(err)
		}
		if _, err := c.Check(ctx, project(), Options{}); err != nil {
			b.Fatal(err)
		}
		c.Close(ctx)
	}
}

// Warm is the per-edit cost an agent actually waits on: one instance, one
// check, the module already compiled.
func BenchmarkTSGoWarm(b *testing.B) {
	ctx := context.Background()
	c, err := NewTSGo(ctx, Config{Blob: benchBlob(b, envTSGo, envTSGoSum), Cache: cacheDir})
	if err != nil {
		b.Fatal(err)
	}
	defer c.Close(ctx)
	if _, err := c.Check(ctx, project(), Options{}); err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		if _, err := c.Check(ctx, project(), Options{}); err != nil {
			b.Fatal(err)
		}
	}
}

// Eight at once out of one compiled module: the throughput a session-per-agent
// host gets, and the memory it pays for it. ns/op is the wall time for all
// eight; ms/check divides it.
func BenchmarkTSGoFleet(b *testing.B) {
	const n = 8
	ctx := context.Background()
	c, err := NewTSGo(ctx, Config{Blob: benchBlob(b, envTSGo, envTSGoSum), Cache: cacheDir})
	if err != nil {
		b.Fatal(err)
	}
	defer c.Close(ctx)
	if _, err := c.Check(ctx, project(), Options{}); err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		var wg sync.WaitGroup
		errs := make([]error, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, errs[i] = c.Check(ctx, project(), Options{})
			}(i)
		}
		wg.Wait()
		for _, err := range errs {
			if err != nil {
				b.Fatal(err)
			}
		}
	}
	b.StopTimer()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	b.ReportMetric(float64(b.Elapsed().Milliseconds())/float64(b.N*n), "ms/check")
	b.ReportMetric(float64(ms.Sys)/(1<<20), "MiB-host")
}

// A bundle is a session: start the service, ask once, let it exit. The 2-3ms
// incremental number belongs to a kept session and is not what this measures.
func BenchmarkEsbuildCold(b *testing.B) {
	ctx := context.Background()
	blob := benchBlob(b, envEsbuild, envEsbSum)
	warm(b, blob)
	for b.Loop() {
		e, err := NewEsbuild(ctx, Config{Blob: blob, Cache: cacheDir})
		if err != nil {
			b.Fatal(err)
		}
		if _, err := e.Bundle(ctx, three(), BundleOptions{}); err != nil {
			b.Fatal(err)
		}
		e.Close(ctx)
	}
}

func BenchmarkEsbuildWarm(b *testing.B) {
	ctx := context.Background()
	e, err := NewEsbuild(ctx, Config{Blob: benchBlob(b, envEsbuild, envEsbSum), Cache: cacheDir})
	if err != nil {
		b.Fatal(err)
	}
	defer e.Close(ctx)
	if _, err := e.Bundle(ctx, three(), BundleOptions{}); err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		if _, err := e.Bundle(ctx, three(), BundleOptions{}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEsbuildFleet(b *testing.B) {
	const n = 8
	ctx := context.Background()
	e, err := NewEsbuild(ctx, Config{Blob: benchBlob(b, envEsbuild, envEsbSum), Cache: cacheDir})
	if err != nil {
		b.Fatal(err)
	}
	defer e.Close(ctx)
	if _, err := e.Bundle(ctx, three(), BundleOptions{}); err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		var wg sync.WaitGroup
		errs := make([]error, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, errs[i] = e.Bundle(ctx, three(), BundleOptions{})
			}(i)
		}
		wg.Wait()
		for _, err := range errs {
			if err != nil {
				b.Fatal(err)
			}
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(b.Elapsed().Milliseconds())/float64(b.N*n), "ms/bundle")
}
