package sqlitevec

import (
	"context"
	"path/filepath"
	"testing"
)

// TestGlobalClientSettingsToleratesUnknownField writes a raw meta blob (as if
// an older binary were reading a value a newer writer produced) containing a
// field client.ClientSettings does not know about, and checks
// GlobalClientSettings loads it without error, keeping every recognized
// field. Unlike the round-trip conformance coverage (which only ever writes
// through SetGlobalClientSettings, so it can never smuggle in an unknown
// key), this pokes the stored blob directly to prove the actual persisted
// JSON — not just the Go type — decodes tolerantly.
func TestGlobalClientSettingsToleratesUnknownField(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "unknown-field.db"), 8)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	raw := `{"recall_limit": 9, "a_field_this_binary_has_never_heard_of": {"nested": true}}`
	if _, err := st.db.ExecContext(ctx,
		`INSERT INTO meta(key, value) VALUES(?, ?)`, metaClientSettingsDefaults, raw); err != nil {
		t.Fatalf("seed raw meta row: %v", err)
	}

	got, err := st.GlobalClientSettings(ctx)
	if err != nil {
		t.Fatalf("GlobalClientSettings with an unknown field in the stored blob: %v", err)
	}
	if got.RecallLimit == nil || *got.RecallLimit != 9 {
		t.Fatalf("recall_limit = %v, want 9", got.RecallLimit)
	}
}

// TestAPIKeySettingsToleratesUnknownField is the same guarantee for the
// per-key settings column (api_keys.settings), reached through
// GetAPIKeyByHash and ListAPIKeys.
func TestAPIKeySettingsToleratesUnknownField(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "unknown-field-key.db"), 8)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	rawSettings := `{"auto_save": false, "some_future_client_field": ["a", "b"]}`
	if _, err := st.db.ExecContext(ctx,
		`INSERT INTO api_keys (name, key_hash, home_ns, default_ns, created_at, disabled, settings)
		 VALUES (?,?,?,?,?,?,?)`,
		"legacy-reader", "deadbeef", "", "", "2026-01-01T00:00:00Z", 0, rawSettings,
	); err != nil {
		t.Fatalf("seed raw api_keys row: %v", err)
	}

	got, err := st.GetAPIKeyByHash(ctx, "deadbeef")
	if err != nil {
		t.Fatalf("GetAPIKeyByHash with an unknown field in settings: %v", err)
	}
	if got == nil {
		t.Fatalf("GetAPIKeyByHash: got nil")
	}
	if got.Settings.AutoSave == nil || *got.Settings.AutoSave != false {
		t.Fatalf("auto_save = %v, want false", got.Settings.AutoSave)
	}

	all, err := st.ListAPIKeys(ctx)
	if err != nil {
		t.Fatalf("ListAPIKeys with an unknown field in settings: %v", err)
	}
	if len(all) != 1 || all[0].Settings.AutoSave == nil || *all[0].Settings.AutoSave != false {
		t.Fatalf("ListAPIKeys = %+v, want one key with auto_save=false", all)
	}
}
