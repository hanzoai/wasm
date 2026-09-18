// Copyright © 2026 Hanzo AI. MIT License.

package compile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// Blob names a wasm module by where to get it and what it must hash to. tsgo is
// 49 MiB and esbuild 20 MiB, so neither lives in this repository; the host
// fetches them and this type is why a fetch cannot substitute something else.
//
// Sum is required. A module with no declared digest is refused rather than
// trusted, because the one thing a compile host must never be is a way to run
// somebody else's code.
type Blob struct {
	// Path is a local file. It wins when set.
	Path string
	// URL is fetched over http when Path is empty.
	URL string
	// Sum is the hex sha256 the bytes must have.
	Sum string
}

// Bytes fetches the module and verifies it. A digest mismatch names both
// digests: the caller has either a corrupt download or the wrong artifact, and
// those need different fixes.
func (b Blob) Bytes(ctx context.Context) ([]byte, error) {
	if b.Sum == "" {
		return nil, fmt.Errorf("compile: blob has no sha256")
	}
	var (
		raw []byte
		err error
	)
	switch {
	case b.Path != "":
		raw, err = os.ReadFile(b.Path)
		if err != nil {
			return nil, fmt.Errorf("compile: read module: %w", err)
		}
	case b.URL != "":
		raw, err = fetch(ctx, b.URL)
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("compile: blob names neither a path nor a url")
	}
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); !strings.EqualFold(got, b.Sum) {
		return nil, fmt.Errorf("compile: module digest is %s, declared %s", got, b.Sum)
	}
	return raw, nil
}

func fetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("compile: fetch module: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("compile: fetch module: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("compile: fetch module %s: %s", url, resp.Status)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("compile: fetch module %s: %w", url, err)
	}
	return raw, nil
}
