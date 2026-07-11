package apiauth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/eleboucher/memini/internal/httputil"
	"github.com/eleboucher/memini/internal/store"
)

// fileKeysDoc is the on-disk shape of MEMINI_API_KEYS_FILE (see
// config.Config.APIKeysFile's doc for the full format and an example under
// internal/apiauth/testdata/api_keys.example.yaml). One of hash/secret is
// required per entry; home/default_namespace/disabled are optional.
type fileKeysDoc struct {
	Keys []fileKeyEntry `yaml:"keys"`
}

type fileKeyEntry struct {
	Name             string `yaml:"name"`
	Hash             string `yaml:"hash"`
	Secret           string `yaml:"secret"`
	Home             string `yaml:"home"`
	DefaultNamespace string `yaml:"default_namespace"`
	Disabled         bool   `yaml:"disabled"`
}

// FileKeySet is the immutable, in-memory result of loading MEMINI_API_KEYS_FILE
// at boot (see LoadFileKeys). It is consulted by Config.Authenticate after the
// admin key and before the table (store.APIKeyStore) — see Authenticate's doc
// for the full precedence and auth-mode rules. There is deliberately no
// mutation API: the file is the source of truth for these keys, reloaded only
// by restarting the process (a SIGHUP-triggered reload is a reasonable future
// addition but is not built here — GitOps rollouts already restart the pod on
// a file change, so a boot-time load is sufficient for now).
type FileKeySet struct {
	byHash map[string]store.APIKey
	byName map[string]store.APIKey
}

// LoadFileKeys parses and validates the declarative API keys file at path.
// It is meant to run exactly once, at server boot: see FileKeySet's doc for
// why there is no live reload. Every validation failure names path and the
// offending entry (by index and, when available, by name) so an operator can
// fix the file without hunting — callers should treat any error here as fatal
// to boot (fail loud), never as "start with the file's keys disabled".
//
// Validation performed, each a fatal error:
//   - the file must parse as YAML matching the documented shape
//   - every entry needs a non-empty name, unique within the file
//   - every entry needs EXACTLY one of hash (hex-encoded SHA-256 of the
//     secret) or secret (the plaintext, hashed here and never retained —
//     see fileKeyEntry.Secret)
//   - hash, when given, must decode as exactly 32 bytes of hex
//   - home / default_namespace, when given, are normalized
//     (httputil.NormalizeNamespace) and must pass httputil.ValidateNamespace
func LoadFileKeys(path string) (*FileKeySet, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("api keys file %s: %w", path, err)
	}
	var doc fileKeysDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("api keys file %s: invalid YAML: %w", path, err)
	}

	byHash := make(map[string]store.APIKey, len(doc.Keys))
	byName := make(map[string]store.APIKey, len(doc.Keys))
	for i, e := range doc.Keys {
		key, err := validateFileKeyEntry(i, e)
		if err != nil {
			return nil, fmt.Errorf("api keys file %s: %w", path, err)
		}
		if _, dup := byName[key.Name]; dup {
			return nil, fmt.Errorf("api keys file %s: entry #%d (name %q): duplicate name", path, i+1, key.Name)
		}
		byName[key.Name] = key
		byHash[key.Hash] = key
	}
	return &FileKeySet{byHash: byHash, byName: byName}, nil
}

// validateFileKeyEntry validates and converts one YAML entry to its
// store.APIKey shape. idx is the entry's zero-based position in the file,
// used to identify it in error messages before a name is known to be valid.
func validateFileKeyEntry(idx int, e fileKeyEntry) (store.APIKey, error) {
	label := fmt.Sprintf("entry #%d", idx+1)
	name := strings.TrimSpace(e.Name)
	if name == "" {
		return store.APIKey{}, fmt.Errorf("%s: name is required", label)
	}
	label = fmt.Sprintf("entry #%d (name %q)", idx+1, name)

	hasHash := strings.TrimSpace(e.Hash) != ""
	hasSecret := e.Secret != ""
	if hasHash == hasSecret {
		return store.APIKey{}, fmt.Errorf("%s: exactly one of hash or secret is required", label)
	}

	var hash string
	if hasHash {
		hash = strings.ToLower(strings.TrimSpace(e.Hash))
		decoded, herr := hex.DecodeString(hash)
		if herr != nil || len(decoded) != sha256.Size {
			return store.APIKey{}, fmt.Errorf(
				"%s: hash must be a %d-byte hex-encoded SHA-256 (got %d bytes)",
				label, sha256.Size, len(decoded))
		}
	} else {
		// Hashed immediately; the plaintext secret is never retained beyond
		// this call (see FileKeySet's doc: "the file itself is the secret
		// store", not memini's process memory).
		hash = hashToken(e.Secret)
	}

	home := httputil.NormalizeNamespace(e.Home)
	if home != "" {
		if verr := httputil.ValidateNamespace(home); verr != nil {
			return store.APIKey{}, fmt.Errorf("%s: invalid home namespace: %w", label, verr)
		}
	}
	defNS := httputil.NormalizeNamespace(e.DefaultNamespace)
	if defNS != "" {
		if verr := httputil.ValidateNamespace(defNS); verr != nil {
			return store.APIKey{}, fmt.Errorf("%s: invalid default_namespace: %w", label, verr)
		}
	}

	return store.APIKey{
		Name:      name,
		Hash:      hash,
		HomeNS:    home,
		DefaultNS: defNS,
		Disabled:  e.Disabled,
	}, nil
}

// lookup returns the file key whose Hash matches, or ok=false. A nil receiver
// (feature off — MEMINI_API_KEYS_FILE unset) always misses.
func (fk *FileKeySet) lookup(hash string) (store.APIKey, bool) {
	if fk == nil {
		return store.APIKey{}, false
	}
	k, ok := fk.byHash[hash]
	return k, ok
}

// enforced reports whether this set's mere presence must force auth even
// with no admin key and an empty/absent table — see Authenticate's doc. A
// nil set (feature off) or one loaded from a file with zero entries never
// enforces, mirroring the table's own empty-means-dev-mode rule.
func (fk *FileKeySet) enforced() bool {
	return fk != nil && len(fk.byHash) > 0
}

// IsFileKey reports whether name identifies a declaratively managed key. K3b's
// future key-mutation endpoints (rename/rotate/delete) must consult this and
// refuse by name — the file, not the API, owns these keys' identity. A nil
// receiver (feature off) always reports false.
func (fk *FileKeySet) IsFileKey(name string) bool {
	if fk == nil {
		return false
	}
	_, ok := fk.byName[name]
	return ok
}

// FileKeys returns every declaratively managed key ordered by name — the same
// shape as store.APIKeyStore.ListAPIKeys, including the hash (not sensitive:
// ListAPIKeys already exposes it for table keys) but never a plaintext
// secret. This is the seam K3b's read-only key listing consumes to fold file
// keys into its output. A nil receiver (feature off) returns nil.
func (fk *FileKeySet) FileKeys() []store.APIKey {
	if fk == nil {
		return nil
	}
	out := make([]store.APIKey, 0, len(fk.byName))
	for _, k := range fk.byName {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ShadowedDBKeyNames returns the names of DB-stored keys (ks.ListAPIKeys)
// that share a name with a file key — those DB rows still exist, but the
// file wins at auth time (see Authenticate), so they're effectively dead
// weight until removed or the file entry is deleted. Meant to be called once
// at boot, only when a file is configured, so an operator sees a warning
// naming exactly which DB keys are shadowed. fk nil or ks nil (no file
// configured, or no table capability) returns (nil, nil) — nothing to check.
func (fk *FileKeySet) ShadowedDBKeyNames(ctx context.Context, ks store.APIKeyStore) ([]string, error) {
	if fk == nil || ks == nil || len(fk.byName) == 0 {
		return nil, nil
	}
	dbKeys, err := ks.ListAPIKeys(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing db api keys to check for file-key shadowing: %w", err)
	}
	var shadowed []string
	for _, k := range dbKeys {
		if fk.IsFileKey(k.Name) {
			shadowed = append(shadowed, k.Name)
		}
	}
	sort.Strings(shadowed)
	return shadowed, nil
}
