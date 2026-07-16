package store_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/eleboucher/memini/internal/store"
)

// sliceOfLen returns a *[]string of n copies of s, for exercising the
// inject_pretool_tools element-count bound.
func sliceOfLen(n int, s string) *[]string {
	out := make([]string, n)
	for i := range out {
		out[i] = s
	}
	return &out
}

// TestDefaultClientSettings pins every field's built-in default against the
// ClientSettings schema in api/openapi.yaml (config-handshake redesign) — the
// schema wins on any disagreement, so a drift here should be resolved by
// fixing store.DefaultClientSettings, not this test.
func TestDefaultClientSettings(t *testing.T) {
	d := store.DefaultClientSettings()

	boolFields := map[string]*bool{
		"capture_turns":  d.CaptureTurns,
		"session_digest": d.SessionDigest,
		"inline_extract": d.InlineExtract,
		"auto_save":      d.AutoSave,
		"inject_dedupe":  d.InjectDedupe,
		"recall":         d.Recall,
		"capture":        d.Capture,
	}
	for name, v := range boolFields {
		if v == nil || *v != true {
			t.Errorf("%s = %v, want true", name, v)
		}
	}

	intFields := map[string]struct {
		got  *int
		want int
	}{
		"auto_save_interval":          {d.AutoSaveInterval, 10},
		"auto_save_min_events":        {d.AutoSaveMinEvents, 3},
		"inject_briefing_pinned":      {d.InjectBriefingPinned, 5},
		"inject_briefing_facts":       {d.InjectBriefingFacts, 5},
		"inject_briefing_procedures":  {d.InjectBriefingProcedures, 5},
		"inject_briefing_recent":      {d.InjectBriefingRecent, 3},
		"inject_briefing_max_tok":     {d.InjectBriefingMaxTok, 0},
		"inject_pretool_items":        {d.InjectPretoolItems, 3},
		"inject_pretool_max_tok":      {d.InjectPretoolMaxTok, 0},
		"recall_limit":                {d.RecallLimit, 3},
		"inject_recall_max_tok":       {d.InjectRecallMaxTok, 0},
		"min_capture_chars":           {d.MinCaptureChars, 0},
		"capture_user_max_chars":      {d.CaptureUserMaxChars, 1000},
		"capture_assistant_max_chars": {d.CaptureAssistantMaxChars, 3000},
		"request_timeout_ms":          {d.RequestTimeoutMs, 30000},
	}
	for name, f := range intFields {
		if f.got == nil || *f.got != f.want {
			t.Errorf("%s = %v, want %d", name, f.got, f.want)
		}
	}

	floatFields := map[string]*float64{
		"inject_pretool_min_score": d.InjectPretoolMinScore,
		"inject_recall_min_score":  d.InjectRecallMinScore,
	}
	for name, v := range floatFields {
		if v == nil || *v != 0 {
			t.Errorf("%s = %v, want 0", name, v)
		}
	}

	if d.InjectPretoolTools == nil {
		t.Fatal("inject_pretool_tools = nil, want the default tool list")
	}
	wantTools := []string{"Read", "Write", "Edit", "Glob", "Grep"}
	if !equalStrs(*d.InjectPretoolTools, wantTools) {
		t.Errorf("inject_pretool_tools = %v, want %v", *d.InjectPretoolTools, wantTools)
	}
	if d.InjectLabels == nil || len(*d.InjectLabels) != 0 {
		t.Errorf("inject_labels = %v, want an empty (non-nil) slice", d.InjectLabels)
	}
	if d.NamespaceScope == nil || *d.NamespaceScope != "repo" {
		t.Errorf("namespace_scope = %v, want %q", d.NamespaceScope, "repo")
	}
	if d.NamespacePrefix == nil || *d.NamespacePrefix != "" {
		t.Errorf("namespace_prefix = %v, want empty string", d.NamespacePrefix)
	}

	if err := d.Validate(); err != nil {
		t.Errorf("default settings must validate cleanly: %v", err)
	}
}

// TestDefaultClientSettingsCoversEveryField makes the promise above structural.
// TestDefaultClientSettings checks a hand-written list, so a new ClientSettings
// field whose default nobody wrote passes it silently — and a nil survives into
// a "fully resolved" settings value, which ClientSettings' own doc says can
// never happen and which every consumer of /v1/handshake dereferences. This
// asserts the same thing by reflection, so the failure lands here, next to the
// fix, rather than in a client.
func TestDefaultClientSettingsCoversEveryField(t *testing.T) {
	v := reflect.ValueOf(store.DefaultClientSettings())
	for i, tp := 0, v.Type(); i < tp.NumField(); i++ {
		f := tp.Field(i)
		if !f.IsExported() {
			continue
		}
		if v.Field(i).Kind() != reflect.Pointer {
			t.Errorf("ClientSettings.%s is %s, want a pointer: nil is how the "+
				"merge layers spell \"inherit\"", f.Name, f.Type)
			continue
		}
		if v.Field(i).IsNil() {
			t.Errorf("DefaultClientSettings leaves ClientSettings.%s (json %q) nil — "+
				"every field needs a built-in default. TestDefaultClientSettingsMatchSchema "+
				"separately checks that the value equals the field's `default:` in "+
				"api/openapi.yaml.",
				f.Name, strings.Split(f.Tag.Get("json"), ",")[0])
		}
	}
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestClientSettingsValidate(t *testing.T) {
	tests := []struct {
		name    string
		s       store.ClientSettings
		wantErr bool
	}{
		{"zero value is valid (nothing set)", store.ClientSettings{}, false},
		{"defaults are valid", store.DefaultClientSettings(), false},
		{"auto_save_interval = 1 is valid (boundary)", store.ClientSettings{AutoSaveInterval: new(1)}, false},
		{"auto_save_interval = 0 is invalid", store.ClientSettings{AutoSaveInterval: new(0)}, true},
		{"auto_save_interval negative is invalid", store.ClientSettings{AutoSaveInterval: new(-1)}, true},
		{"auto_save_min_events = 0 is valid (boundary)", store.ClientSettings{AutoSaveMinEvents: new(0)}, false},
		{"auto_save_min_events negative is invalid", store.ClientSettings{AutoSaveMinEvents: new(-1)}, true},
		{"inject_briefing_pinned = 0 is valid (boundary)", store.ClientSettings{InjectBriefingPinned: new(0)}, false},
		{"inject_briefing_pinned negative is invalid", store.ClientSettings{InjectBriefingPinned: new(-1)}, true},
		{"inject_briefing_facts negative is invalid", store.ClientSettings{InjectBriefingFacts: new(-1)}, true},
		{"inject_briefing_procedures negative is invalid", store.ClientSettings{InjectBriefingProcedures: new(-1)}, true},
		{"inject_briefing_recent negative is invalid", store.ClientSettings{InjectBriefingRecent: new(-1)}, true},
		{"inject_briefing_max_tok negative is invalid", store.ClientSettings{InjectBriefingMaxTok: new(-1)}, true},
		{"inject_pretool_items negative is invalid", store.ClientSettings{InjectPretoolItems: new(-1)}, true},
		{"inject_pretool_max_tok negative is invalid", store.ClientSettings{InjectPretoolMaxTok: new(-1)}, true},
		{"recall_limit negative is invalid", store.ClientSettings{RecallLimit: new(-1)}, true},
		{"inject_recall_max_tok negative is invalid", store.ClientSettings{InjectRecallMaxTok: new(-1)}, true},
		{"min_capture_chars negative is invalid", store.ClientSettings{MinCaptureChars: new(-1)}, true},
		{"request_timeout_ms = 100 is valid (boundary)", store.ClientSettings{RequestTimeoutMs: new(100)}, false},
		{"request_timeout_ms = 99 is invalid", store.ClientSettings{RequestTimeoutMs: new(99)}, true},
		// 0 is the tempting "no timeout" spelling, but a client with no timeout
		// hangs forever on a wedged server rather than failing soft, so the
		// schema's minimum rejects it rather than overloading it as "unbounded".
		{"request_timeout_ms = 0 is invalid", store.ClientSettings{RequestTimeoutMs: new(0)}, true},
		{"request_timeout_ms negative is invalid", store.ClientSettings{RequestTimeoutMs: new(-1)}, true},
		{"inject_pretool_min_score = 0 is valid (boundary)", store.ClientSettings{InjectPretoolMinScore: new(0.0)}, false},
		{"inject_pretool_min_score negative is invalid", store.ClientSettings{InjectPretoolMinScore: new(-0.1)}, true},
		{"inject_recall_min_score negative is invalid", store.ClientSettings{InjectRecallMinScore: new(-0.1)}, true},
		{"namespace_scope repo is valid", store.ClientSettings{NamespaceScope: new("repo")}, false},
		{"namespace_scope owner_repo is valid", store.ClientSettings{NamespaceScope: new("owner_repo")}, false},
		{"namespace_scope bad value is invalid", store.ClientSettings{NamespaceScope: new("global")}, true},
		{"inject_labels valid values", store.ClientSettings{InjectLabels: &[]string{"tier", "confidence", "age", "reason"}}, false},
		{"inject_labels empty slice is valid", store.ClientSettings{InjectLabels: &[]string{}}, false},
		{"inject_labels bad value is invalid", store.ClientSettings{InjectLabels: &[]string{"tier", "bogus"}}, true},
		{"namespace_prefix empty is valid", store.ClientSettings{NamespacePrefix: new("")}, false},
		{"namespace_prefix valid path", store.ClientSettings{NamespacePrefix: new("acme/team")}, false},
		{"namespace_prefix with NUL byte is invalid", store.ClientSettings{NamespacePrefix: new("acme\x00team")}, true},
		{"namespace_prefix over 256 bytes is invalid", store.ClientSettings{NamespacePrefix: new(strings.Repeat("a", 257))}, true},
		{"inject_pretool_tools within bounds is valid", store.ClientSettings{InjectPretoolTools: &[]string{"Read", "Write"}}, false},
		{"inject_pretool_tools with 64 entries is valid (boundary)", store.ClientSettings{InjectPretoolTools: sliceOfLen(64, "t")}, false},
		{"inject_pretool_tools over 64 entries is invalid", store.ClientSettings{InjectPretoolTools: sliceOfLen(65, "t")}, true},
		{"inject_pretool_tools with an over-128-char entry is invalid", store.ClientSettings{InjectPretoolTools: &[]string{strings.Repeat("a", 129)}}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.s.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("Validate() = nil, want an error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
		})
	}
}

// TestMergeClientSettings covers MergeClientSettings' per-field precedence
// (later layers win, nil never overrides) and the sources map's per-field
// provenance attribution.
func TestMergeClientSettings(t *testing.T) {
	defaults := store.SettingsLayer{Source: "default", S: store.DefaultClientSettings()}

	t.Run("no layers yields the zero value and an empty sources map", func(t *testing.T) {
		got, sources := store.MergeClientSettings()
		if got != (store.ClientSettings{}) {
			t.Fatalf("merge with no layers = %+v, want the zero value", got)
		}
		if len(sources) != 0 {
			t.Fatalf("sources with no layers = %v, want empty", sources)
		}
	})

	t.Run("defaults-only layer produces a fully non-nil result attributed to default", func(t *testing.T) {
		got, sources := store.MergeClientSettings(defaults)
		if got != defaults.S {
			t.Fatalf("merge(defaults) = %+v, want exactly the defaults", got)
		}
		if sources["capture_turns"] != "default" || sources["namespace_prefix"] != "default" {
			t.Fatalf("sources = %v, want every field attributed to %q", sources, "default")
		}
	})

	t.Run("a later layer's explicit field wins over an earlier layer's", func(t *testing.T) {
		global := store.SettingsLayer{Source: "global", S: store.ClientSettings{
			AutoSaveInterval:  new(20),
			AutoSaveMinEvents: new(5), // untouched by key, must survive from global
		}}
		key := store.SettingsLayer{Source: "key:ci-bot", S: store.ClientSettings{
			AutoSaveInterval: new(99), // overrides global's 20
			Recall:           new(false),
		}}
		got, sources := store.MergeClientSettings(defaults, global, key)

		if got.AutoSaveInterval == nil || *got.AutoSaveInterval != 99 {
			t.Fatalf("auto_save_interval = %v, want 99 (the last layer to set it)", got.AutoSaveInterval)
		}
		if sources["auto_save_interval"] != "key:ci-bot" {
			t.Fatalf("sources[auto_save_interval] = %q, want %q", sources["auto_save_interval"], "key:ci-bot")
		}
		// auto_save_min_events was set only by global; the key layer left it nil,
		// so global's value and provenance must win.
		if got.AutoSaveMinEvents == nil || *got.AutoSaveMinEvents != 5 {
			t.Fatalf("auto_save_min_events = %v, want 5 (global's value, untouched by key)", got.AutoSaveMinEvents)
		}
		if sources["auto_save_min_events"] != "global" {
			t.Fatalf("sources[auto_save_min_events] = %q, want %q", sources["auto_save_min_events"], "global")
		}
		if got.Recall == nil || *got.Recall != false {
			t.Fatalf("recall = %v, want false", got.Recall)
		}
		if sources["recall"] != "key:ci-bot" {
			t.Fatalf("sources[recall] = %q, want %q", sources["recall"], "key:ci-bot")
		}
		// A field neither global nor key touched falls through to defaults'
		// value and provenance.
		if got.RecallLimit == nil || *got.RecallLimit != 3 {
			t.Fatalf("recall_limit = %v, want the default 3", got.RecallLimit)
		}
		if sources["recall_limit"] != "default" {
			t.Fatalf("sources[recall_limit] = %q, want %q", sources["recall_limit"], "default")
		}
		// Every field must be non-nil once the defaults layer is included.
		if got.CaptureTurns == nil || got.NamespacePrefix == nil || got.InjectLabels == nil {
			t.Fatalf("merge with defaults first must leave every field non-nil: %+v", got)
		}
	})

	// The reason request_timeout_ms is a server-pushed setting at all: the admin
	// who deploys a slow cross-encoder raises the client ceiling once, globally,
	// instead of asking every user to export MEMINI_TIMEOUT_MS. A key that talks
	// to an even slower backend can then raise it further on top.
	t.Run("request_timeout_ms layers global-then-key like any other field", func(t *testing.T) {
		global := store.SettingsLayer{Source: "global", S: store.ClientSettings{
			RequestTimeoutMs: new(30000), // the fleet runs a slow reranker
		}}
		got, sources := store.MergeClientSettings(defaults, global)
		if got.RequestTimeoutMs == nil || *got.RequestTimeoutMs != 30000 {
			t.Fatalf("request_timeout_ms = %v, want 30000 (the global layer's value)", got.RequestTimeoutMs)
		}
		if sources["request_timeout_ms"] != "global" {
			t.Fatalf("sources[request_timeout_ms] = %q, want %q", sources["request_timeout_ms"], "global")
		}

		key := store.SettingsLayer{Source: "key:batch", S: store.ClientSettings{RequestTimeoutMs: new(60000)}}
		got, sources = store.MergeClientSettings(defaults, global, key)
		if got.RequestTimeoutMs == nil || *got.RequestTimeoutMs != 60000 {
			t.Fatalf("request_timeout_ms = %v, want 60000 (the key layer's value)", got.RequestTimeoutMs)
		}
		if sources["request_timeout_ms"] != "key:batch" {
			t.Fatalf("sources[request_timeout_ms] = %q, want %q", sources["request_timeout_ms"], "key:batch")
		}
	})

	t.Run("an explicit nil never overrides an earlier explicit value", func(t *testing.T) {
		first := store.SettingsLayer{Source: "global", S: store.ClientSettings{AutoSave: new(false)}}
		second := store.SettingsLayer{Source: "key:bot", S: store.ClientSettings{}} // touches nothing
		got, sources := store.MergeClientSettings(first, second)
		if got.AutoSave == nil || *got.AutoSave != false {
			t.Fatalf("auto_save = %v, want false (untouched by the empty second layer)", got.AutoSave)
		}
		if sources["auto_save"] != "global" {
			t.Fatalf("sources[auto_save] = %q, want %q (the empty layer must not steal provenance)", sources["auto_save"], "global")
		}
	})
}

// TestClientSettingsUnmarshalIgnoresUnknownFields pins the "STORED blob is
// decoded tolerantly" rule from the config-handshake redesign brief: both
// drivers persist ClientSettings by json.Marshal/Unmarshal on this exact Go
// type with no custom (Un)MarshalJSON, so proving encoding/json's default
// unknown-field tolerance here is equivalent to proving it for every driver's
// stored blob — a newer writer's extra field must never break an older
// reader. Strict decoding (rejecting unknown fields) is the REST boundary's
// job in a later phase, not the store's.
func TestClientSettingsUnmarshalIgnoresUnknownFields(t *testing.T) {
	raw := `{"capture_turns": true, "recall_limit": 7, "some_future_field": {"nested": [1,2,3]}}`
	var s store.ClientSettings
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("unmarshal with an unknown field: %v", err)
	}
	if s.CaptureTurns == nil || !*s.CaptureTurns {
		t.Fatalf("capture_turns = %v, want true", s.CaptureTurns)
	}
	if s.RecallLimit == nil || *s.RecallLimit != 7 {
		t.Fatalf("recall_limit = %v, want 7", s.RecallLimit)
	}
}

// TestClientSettingsMarshalOmitsNilFields pins the "only explicitly-set
// fields are persisted" rule at the type level: pointer fields + omitempty
// must drop nil fields (including the numeric-zero-vs-unset distinction,
// since a non-nil pointer to 0 is a real explicit "0", not an omission).
func TestClientSettingsMarshalOmitsNilFields(t *testing.T) {
	s := store.ClientSettings{
		AutoSave:             new(false), // explicit false must survive
		InjectBriefingMaxTok: new(0),     // explicit 0 must survive, not be omitted
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}
	if len(raw) != 2 {
		t.Fatalf("marshaled fields = %v, want exactly auto_save and inject_briefing_max_tok", raw)
	}
	if v, ok := raw["auto_save"]; !ok || v != false {
		t.Fatalf("auto_save = %v (present=%v), want false", v, ok)
	}
	if v, ok := raw["inject_briefing_max_tok"]; !ok || v != float64(0) {
		t.Fatalf("inject_briefing_max_tok = %v (present=%v), want 0 (explicit zero must not be omitted)", v, ok)
	}
}
