package apiauth_test

import (
	"strings"
	"testing"

	"github.com/eleboucher/memini/internal/apiauth"
)

// TestLoadFileKeysParsesSettings pins the per-key ClientSettings override
// (config-handshake redesign): the snake_case wire keys under `settings:` are
// carried onto store.APIKey.Settings, and only the fields set are populated —
// an omitted field stays nil so it keeps inheriting the global/built-in layer.
func TestLoadFileKeysParsesSettings(t *testing.T) {
	path := writeKeysFile(t, `
keys:
  - name: alex
    secret: "tok-alex"
    settings:
      capture_turns: false
      recall_limit: 5
      namespace_scope: owner_repo
      inject_pretool_min_score: 0.25
`)
	fk, err := apiauth.LoadFileKeys(path)
	if err != nil {
		t.Fatalf("LoadFileKeys: %v", err)
	}
	keys := fk.FileKeys()
	if len(keys) != 1 {
		t.Fatalf("want 1 key, got %d", len(keys))
	}
	s := keys[0].Settings
	if s.CaptureTurns == nil || *s.CaptureTurns {
		t.Errorf("capture_turns = %v, want a set false", s.CaptureTurns)
	}
	if s.RecallLimit == nil || *s.RecallLimit != 5 {
		t.Errorf("recall_limit = %v, want 5", s.RecallLimit)
	}
	if s.NamespaceScope == nil || *s.NamespaceScope != "owner_repo" {
		t.Errorf("namespace_scope = %v, want owner_repo", s.NamespaceScope)
	}
	// Non-integral float must survive the YAML->JSON bridge intact.
	if s.InjectPretoolMinScore == nil || *s.InjectPretoolMinScore != 0.25 {
		t.Errorf("inject_pretool_min_score = %v, want 0.25", s.InjectPretoolMinScore)
	}
	// An omitted field stays nil (inherit), never defaulted at parse time.
	if s.SessionDigest != nil {
		t.Errorf("session_digest = %v, want nil (omitted -> inherit)", s.SessionDigest)
	}
}

// TestLoadFileKeysNoSettingsIsZeroOverride pins that a key with no settings
// block carries the zero ClientSettings (every field nil) — no override at all,
// the feature-off no-op.
func TestLoadFileKeysNoSettingsIsZeroOverride(t *testing.T) {
	path := writeKeysFile(t, `
keys:
  - name: plain
    secret: "tok-plain"
`)
	fk, err := apiauth.LoadFileKeys(path)
	if err != nil {
		t.Fatalf("LoadFileKeys: %v", err)
	}
	s := fk.FileKeys()[0].Settings
	if s.CaptureTurns != nil || s.RecallLimit != nil || s.NamespaceScope != nil {
		t.Errorf("a key with no settings block must have a zero-value override, got %+v", s)
	}
}

// TestLoadFileKeysInvalidSettingsFatal pins the fail-loud boot: a per-key
// settings block that fails ClientSettings.Validate, or carries an unknown key,
// refuses the boot — naming the file and the offending entry, never a silently
// dropped setting.
func TestLoadFileKeysInvalidSettingsFatal(t *testing.T) {
	cases := []struct {
		name, block string
	}{
		{"out-of-range value", "      auto_save_interval: 0\n"},
		{"bad enum value", "      namespace_scope: nonsense\n"},
		{"unknown key", "      bogus_field: true\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := writeKeysFile(t, `
keys:
  - name: badbot
    secret: "tok-badbot"
    settings:
`+c.block)
			_, err := apiauth.LoadFileKeys(path)
			if err == nil {
				t.Fatalf("LoadFileKeys should be fatal for %s", c.name)
			}
			assertErrNamesFile(t, err, path)
			if !strings.Contains(err.Error(), "badbot") {
				t.Errorf("error should name the offending entry, got: %v", err)
			}
		})
	}
}
