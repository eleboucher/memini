package store_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/eleboucher/memini/internal/store"
)

// api/openapi.yaml is the declared source of truth for ClientSettings: the
// struct's own doc says "the schema wins on any disagreement". Nothing enforced
// that. The defaults were copied by hand into DefaultClientSettings, into a map
// in clientsettings_test.go, and into BEHAVIOR_KNOBS on the client — four
// copies, each only as right as the last person to update all of them.
//
// These tests read the schema and check the copies against it, so "the schema
// wins" is a fact rather than a comment.

const clientSettingsSchema = "ClientSettings"

// schemaField is one property of the ClientSettings schema.
type schemaField struct {
	Type    string `yaml:"type"`
	Default any    `yaml:"default"`
	Minimum *int   `yaml:"minimum"`
}

// loadClientSettingsSchema returns the ClientSettings properties from the spec,
// keyed by wire (snake_case) name.
func loadClientSettingsSchema(t *testing.T) map[string]schemaField {
	t.Helper()
	path := filepath.Join("..", "..", "api", "openapi.yaml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var spec struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]schemaField `yaml:"properties"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(b, &spec); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	s, ok := spec.Components.Schemas[clientSettingsSchema]
	if !ok {
		t.Fatalf("%s: no %s schema", path, clientSettingsSchema)
	}
	if len(s.Properties) == 0 {
		t.Fatalf("%s: %s has no properties", path, clientSettingsSchema)
	}
	return s.Properties
}

// TestDefaultClientSettingsMatchSchema pins every built-in default against the
// schema's `default:`. TestDefaultClientSettings checks the same thing from a
// hand-written map, which cannot catch a field defaulted to the wrong value
// AND listed with that same wrong value in the map — this can, because it reads
// the spec instead of a second copy of the answer.
func TestDefaultClientSettingsMatchSchema(t *testing.T) {
	props := loadClientSettingsSchema(t)
	d := store.DefaultClientSettings()
	v := reflect.ValueOf(d)

	checked := 0
	for i, tp := 0, v.Type(); i < tp.NumField(); i++ {
		f := tp.Field(i)
		if !f.IsExported() {
			continue
		}
		key := jsonKey(f)
		prop, ok := props[key]
		if !ok {
			t.Errorf("ClientSettings.%s (json %q) has no property in the %s schema — "+
				"the schema is the source of truth, so add it there", f.Name, key, clientSettingsSchema)
			continue
		}
		if prop.Default == nil {
			continue // no declared default; DefaultClientSettings picks one freely
		}
		fv := v.Field(i)
		if fv.IsNil() {
			continue // covered, with a better message, by TestDefaultClientSettingsCoversEveryField
		}
		got := fv.Elem().Interface()
		if !sameScalar(got, prop.Default) {
			t.Errorf("DefaultClientSettings %s = %v, but api/openapi.yaml declares default %v — "+
				"the schema wins; fix the default, not the schema", key, got, prop.Default)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("checked no defaults — the schema parse or the json-tag mapping is broken")
	}
}

// TestBehaviorKnobDefaultsMatchSchema pins the CLIENT's copy of the defaults.
// packages/memini-client/src/settings.ts carries a BEHAVIOR_KNOBS default per
// knob, used whenever a client cannot reach the server — the exact moment a
// stale value goes unnoticed. It is a hand-copied fourth instance of the same
// numbers, so raising a default server-side would otherwise leave every
// offline or handshake-degraded client silently on the old one.
//
// Reading TypeScript from a Go test is unusual, but the alternative is no check
// at all: the client package is deliberately dependency-free and has no YAML
// parser. cmd/gendocs already scrapes Go source for the same reason.
func TestBehaviorKnobDefaultsMatchSchema(t *testing.T) {
	props := loadClientSettingsSchema(t)

	path := filepath.Join("..", "..", "packages", "memini-client", "src", "settings.ts")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	// { envName: "...", wireKey: "x", kind: "int", default: 1000 }
	re := regexp.MustCompile(`wireKey:\s*"([a-z_]+)"[\s\S]{0,120}?default:\s*([^,\n}]+)`)
	matches := re.FindAllStringSubmatch(string(b), -1)
	if len(matches) == 0 {
		t.Fatalf("%s: matched no BEHAVIOR_KNOBS entries — has the shape changed?", path)
	}

	seen := 0
	for _, m := range matches {
		key, raw := m[1], m[2]
		prop, ok := props[key]
		if !ok {
			t.Errorf("%s: knob %q is not a %s property", path, key, clientSettingsSchema)
			continue
		}
		if prop.Default == nil {
			continue
		}
		got, err := parseTSLiteral(raw)
		if err != nil {
			continue // non-scalar default (e.g. a list); the schema check below is scalar-only
		}
		if !sameScalar(got, prop.Default) {
			t.Errorf("%s: BEHAVIOR_KNOBS %q default = %v, but api/openapi.yaml declares %v — "+
				"an offline client would capture at the wrong bound", path, key, got, prop.Default)
		}
		seen++
	}
	if seen == 0 {
		t.Fatal("compared no knob defaults — the regex or the schema parse is broken")
	}
	// The bounds this whole change exists for must be among them.
	for _, must := range []string{"capture_user_max_chars", "capture_assistant_max_chars"} {
		found := false
		for _, m := range matches {
			if m[1] == must {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: no BEHAVIOR_KNOBS entry for %q — the client cannot resolve it", path, must)
		}
	}
}

// jsonKey returns a struct field's wire name from its json tag.
func jsonKey(f reflect.StructField) string {
	tag := f.Tag.Get("json")
	for i := 0; i < len(tag); i++ {
		if tag[i] == ',' {
			return tag[:i]
		}
	}
	return tag
}

// parseTSLiteral reads a TypeScript scalar literal (number, bool, string).
func parseTSLiteral(raw string) (any, error) {
	s := trimSpace(raw)
	switch s {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n, nil
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f, nil
	}
	var str string
	if err := json.Unmarshal([]byte(s), &str); err == nil {
		return str, nil
	}
	return nil, errNotScalar
}

var errNotScalar = &scalarErr{}

type scalarErr struct{}

func (*scalarErr) Error() string { return "not a scalar literal" }

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t' || s[start] == '\n' || s[start] == '\r') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\n' || s[end-1] == '\r') {
		end--
	}
	return s[start:end]
}

// sameScalar compares a Go value against a YAML-decoded one, bridging the
// representations the two sides happen to produce: numeric widths (int vs
// float64) and sequences (a typed []string here vs YAML's []any).
func sameScalar(got, want any) bool {
	gf, gok := toFloat(got)
	wf, wok := toFloat(want)
	if gok && wok {
		return gf == wf
	}
	if gs, ok := toStrings(got); ok {
		ws, ok2 := toStrings(want)
		if !ok2 || len(gs) != len(ws) {
			return false
		}
		for i := range gs {
			if gs[i] != ws[i] {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(got, want)
}

// toStrings normalizes a sequence to []string, whatever element type it
// arrived with. Returns false for anything that is not a slice/array.
func toStrings(v any) ([]string, bool) {
	rv := reflect.ValueOf(v)
	if !rv.IsValid() || (rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array) {
		return nil, false
	}
	out := make([]string, rv.Len())
	for i := range out {
		e := rv.Index(i).Interface()
		s, ok := e.(string)
		if !ok {
			return nil, false
		}
		out[i] = s
	}
	return out, true
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	case float32:
		return float64(n), true
	}
	return 0, false
}
