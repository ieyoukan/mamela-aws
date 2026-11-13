// Package models keeps AWS's models the catalogs come from: it reads models.lock.json, downloads
// the pinned files into a cache (only from the pinned commit of AWS's repository, each checked
// against its sha256), and loads them. The catalogs package does no I/O; the provider program
// never imports this one.
package models

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"

	"github.com/ieyoukan/mamela-aws/catalogs"
)

// ErrNotCached: a pinned model is not in the cache (run the fetch).
var ErrNotCached = errors.New("not in the cache")

// ReadLock reads a lock file.
func ReadLock(path string) (catalogs.Lock, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return catalogs.Lock{}, err
	}
	l, err := catalogs.ParseLock(data)
	if err != nil {
		return l, fmt.Errorf("%s: %w", path, err)
	}
	return l, nil
}

// WriteLock writes a lock file.
func WriteLock(path string, l catalogs.Lock) error {
	data, err := l.Encode()
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// Load reads the pinned models from the cache, checking each one's sha256.
func Load(l catalogs.Lock, dir string) (map[string]*catalogs.Model, error) {
	out := map[string]*catalogs.Model{}
	for _, name := range l.Names() {
		pin := l.Models[name]
		path := filepath.Join(dir, pin.CacheName())
		data, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("model %s (%s): %w %s: download the pinned models with `go run ./cmd/aws-catalogs fetch`", name, pin.Path, ErrNotCached, dir)
		}
		if err != nil {
			return nil, err
		}
		if !pin.Pinned(data) {
			return nil, fmt.Errorf("model %s in the cache (%s) is not the pinned one (sha256 %s): remove it and fetch again", name, path, pin.SHA256)
		}
		m, err := catalogs.LoadModel(data)
		if err != nil {
			return nil, fmt.Errorf("model %s: %w", name, err)
		}
		out[name] = m
	}
	return out, nil
}

// Fetch downloads the pinned models that are not in the cache yet, from the pinned commit of
// AWS's repository, and keeps each only if its sha256 is the pinned one. It says what it does on
// log.
func Fetch(ctx context.Context, l catalogs.Lock, dir string, log io.Writer) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, name := range l.Names() {
		pin := l.Models[name]
		path := filepath.Join(dir, pin.CacheName())
		if data, err := os.ReadFile(path); err == nil && pin.Pinned(data) {
			fmt.Fprintf(log, "%-5s %s: in the cache\n", name, pin.Path)
			continue
		}
		data, err := download(ctx, pin.URL())
		if err != nil {
			return fmt.Errorf("model %s: %w", name, err)
		}
		if !pin.Pinned(data) {
			return fmt.Errorf("model %s: %s has sha256 %s, not the pinned %s: nothing is kept", name, pin.URL(), catalogs.Sum(data), pin.SHA256)
		}
		if err := writeAtomic(path, data); err != nil {
			return err
		}
		fmt.Fprintf(log, "%-5s %s: downloaded (%d bytes, sha256 ok)\n", name, pin.Path, len(data))
	}
	return nil
}

// Relock pins every model to another commit of the repository (the same files): it downloads
// each, puts it in the cache and records its sha256. Review what the regenerated catalogs change
// before committing the lock.
func Relock(ctx context.Context, l catalogs.Lock, commit, dir string, log io.Writer) (catalogs.Lock, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return l, err
	}
	out := catalogs.Lock{Schema: catalogs.LockSchema, Models: map[string]catalogs.Locked{}}
	for _, name := range l.Names() {
		pin := l.Models[name]
		pin.Commit = commit
		if err := pin.Check(); err != nil {
			return l, err
		}
		data, err := download(ctx, pin.URL())
		if err != nil {
			return l, fmt.Errorf("model %s: %w", name, err)
		}
		pin.SHA256 = catalogs.Sum(data)
		if err := writeAtomic(filepath.Join(dir, pin.CacheName()), data); err != nil {
			return l, err
		}
		fmt.Fprintf(log, "%-5s %s @ %s: sha256 %s\n", name, pin.Path, commit[:12], pin.SHA256)
		out.Models[name] = pin
	}
	return out, nil
}

func download(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "mamela-aws-catalogs")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64<<20))
}

func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
