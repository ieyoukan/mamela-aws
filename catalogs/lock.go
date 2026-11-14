package catalogs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"regexp"
	"strings"
)

// The models the catalogs come from are pinned in models.lock.json: for each, the repository,
// the commit, the file and its sha256. They are not committed: `go run ./cmd/aws-catalogs fetch`
// downloads them, from the pinned commit only, into a cache (git ignores it), and every use checks
// their sha256. (This package does no I/O: the provider program imports it.)

// LockSchema is the schema of models.lock.json.
const LockSchema = "mamela-aws-models/v1"

// Source is the only repository models are taken from: AWS's own (Apache-2.0).
const Source = "github.com/aws/api-models-aws"

// Lock is models.lock.json.
type Lock struct {
	Schema string `json:"schema"`
	// Models are by name (as API.Model names them).
	Models map[string]Locked `json:"models"`
}

// Locked is one pinned model.
type Locked struct {
	Repo   string `json:"repo"`
	Commit string `json:"commit"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

var (
	commitRe = regexp.MustCompile(`^[0-9a-f]{40}$`)
	pathRe   = regexp.MustCompile(`^models/[a-z0-9-]+/service/[0-9-]+/[a-z0-9-]+\.json$`)
	sumRe    = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ParseLock reads a lock file, and refuses one that names another source, or no exact commit,
// file or sha256, or misses a model of a catalog.
func ParseLock(data []byte) (Lock, error) {
	var l Lock
	if err := json.Unmarshal(data, &l, json.RejectUnknownMembers(true)); err != nil {
		return l, err
	}
	if l.Schema != LockSchema {
		return l, fmt.Errorf("schema %q is not %s", l.Schema, LockSchema)
	}
	for _, a := range APIs {
		if _, ok := l.Models[a.Model]; !ok {
			return l, fmt.Errorf("no model %q (of catalog %s)", a.Model, a.Name)
		}
	}
	for _, name := range sortedKeys(l.Models) {
		if err := l.Models[name].Check(); err != nil {
			return l, fmt.Errorf("model %s: %w", name, err)
		}
	}
	return l, nil
}

// Check checks a pin: AWS's repository, a full commit ID, a model's path, a sha256.
func (m Locked) Check() error {
	switch {
	case m.Repo != Source:
		return fmt.Errorf("repo %q: models come from %s only", m.Repo, Source)
	case !commitRe.MatchString(m.Commit):
		return fmt.Errorf("commit %q is not a full commit ID", m.Commit)
	case !pathRe.MatchString(m.Path):
		return fmt.Errorf("path %q is not a model of the repository", m.Path)
	case !sumRe.MatchString(m.SHA256):
		return fmt.Errorf("sha256 %q", m.SHA256)
	}
	return nil
}

// URL is where the pinned file is downloaded from.
func (m Locked) URL() string {
	return "https://raw.githubusercontent.com/" + strings.TrimPrefix(m.Repo, "github.com/") + "/" + m.Commit + "/" + m.Path
}

// CacheName is the model's file name in the cache: its sha256, so that another pin is another file.
func (m Locked) CacheName() string { return m.SHA256 + ".json" }

// Pinned says whether data is the pinned file.
func (m Locked) Pinned(data []byte) bool { return Sum(data) == m.SHA256 }

// Sum is data's sha256, in hex.
func Sum(data []byte) string {
	s := sha256.Sum256(data)
	return hex.EncodeToString(s[:])
}

// Encode writes the lock file, deterministic.
func (l Lock) Encode() ([]byte, error) {
	data, err := json.Marshal(l, json.Deterministic(true), jsontext.Multiline(true), jsontext.WithIndent("  "))
	return append(data, '\n'), err
}

// Names are the lock's models, in order.
func (l Lock) Names() []string { return sortedKeys(l.Models) }
