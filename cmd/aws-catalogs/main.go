// Command aws-catalogs makes the provider's API catalogs (mamela-catalog/v1) from AWS's official
// Smithy models, pinned in catalogs/models.lock.json, and the reviewed overlay (catalogs/overlay.go):
//
//	go run ./cmd/aws-catalogs fetch               # download the pinned models into the cache (sha256 checked)
//	go run ./cmd/aws-catalogs generate pome/catalogs
//	go run ./cmd/aws-catalogs relock <commit>     # pin the models of another commit of aws/api-models-aws
//
// generate also checks every call the provider declares against the models. The cache is
// .cache/aws-models (MAMELA_AWS_MODELS sets another); only fetch and relock use the network, and
// only to download the pinned files from AWS's repository.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ieyoukan/mamela-aws/aws"
	"github.com/ieyoukan/mamela-aws/catalogs"
	"github.com/ieyoukan/mamela-aws/catalogs/models"
	"github.com/ieyoukan/mamela/provider/protocol"
)

const lockPath = "catalogs/models.lock.json"

const usage = `usage: aws-catalogs fetch | generate [--check] <dir> | relock <commit>`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "aws-catalogs:", err)
		os.Exit(1)
	}
}

// Cache is where the models are kept.
func Cache() string {
	if d := os.Getenv("MAMELA_AWS_MODELS"); d != "" {
		return d
	}
	return filepath.Join(".cache", "aws-models")
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	lock, err := models.ReadLock(lockPath)
	if err != nil {
		return fmt.Errorf("%w (run it in the repository's root)", err)
	}
	ctx := context.Background()
	switch {
	case args[0] == "fetch" && len(args) == 1:
		return models.Fetch(ctx, lock, Cache(), os.Stdout)
	case args[0] == "relock" && len(args) == 2:
		l, err := models.Relock(ctx, lock, args[1], Cache(), os.Stdout)
		if err != nil {
			return err
		}
		if err := models.WriteLock(lockPath, l); err != nil {
			return err
		}
		fmt.Printf("wrote %s: now regenerate (generate pome/catalogs), read the diff of pome/catalogs, and run the tests\n", lockPath)
		return nil
	case args[0] == "generate" && (len(args) == 2 || len(args) == 3 && args[1] == "--check"):
		return generate(lock, args[len(args)-1], len(args) == 3)
	}
	return errors.New(usage)
}

// generate writes the catalogs into dir (or, with check, says whether they are what is there).
func generate(lock catalogs.Lock, dir string, check bool) error {
	ms, err := models.Load(lock, Cache())
	if err != nil {
		return err
	}
	cats, err := catalogs.Generate(ms)
	if err != nil {
		return fmt.Errorf("the catalogs do not follow from the models:\n%w", err)
	}
	// Every call the provider declares is one of the models', with parameters of the models'.
	byAPI := map[string]*catalogs.Model{}
	for _, a := range catalogs.APIs {
		byAPI[a.Name] = ms[a.Model]
	}
	if err := catalogs.CheckCalls(byAPI, declared()); err != nil {
		return fmt.Errorf("the provider declares calls the models do not have:\n%w", err)
	}
	var stale []string
	for _, name := range catalogs.Names() {
		data, err := catalogs.Encode(cats[name])
		if err != nil {
			return fmt.Errorf("catalog %s: %w", name, err)
		}
		path := filepath.Join(dir, name+".json")
		if check {
			if got, _ := os.ReadFile(path); !bytes.Equal(got, data) {
				stale = append(stale, path)
			}
			continue
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return err
		}
	}
	if len(stale) > 0 {
		return fmt.Errorf("not what the models and the overlay give: %v (go run ./cmd/aws-catalogs generate %s)", stale, dir)
	}
	return nil
}

// declared are the calls the provider declares: observing, the guard's, and every operation's.
func declared() []protocol.Call {
	m := aws.Provider.Manifest()
	var out []protocol.Call
	add := func(cs []protocol.Call) { out = append(out, cs...) }
	add(m.ObserveCalls)
	if m.Guard != nil {
		add(m.Guard.Calls)
	}
	for _, k := range m.Kinds {
		for _, o := range k.Operations {
			add(o.Calls)
		}
	}
	return out
}
