package llm

import (
	"reflect"
	"testing"
)

func TestSchemaProps(t *testing.T) {
	nested := map[string]any{
		"query": map[string]any{"type": "string"},
		"filters": map[string]any{
			"type":       "object",
			"properties": map[string]any{"tier": map[string]any{"type": "string"}},
		},
	}
	tests := []struct {
		name         string
		schema       map[string]any
		wantProps    any
		wantRequired []string
	}{
		{
			name: "required as []any (post-JSON-decode shape)",
			schema: map[string]any{
				"type":       "object",
				"properties": nested,
				"required":   []any{"query", "filters"},
			},
			wantProps:    nested,
			wantRequired: []string{"query", "filters"},
		},
		{
			name: "required as []string (hand-built schema)",
			schema: map[string]any{
				"properties": nested,
				"required":   []string{"query"},
			},
			wantProps:    nested,
			wantRequired: []string{"query"},
		},
		{
			name: "non-string required entries are skipped",
			schema: map[string]any{
				"properties": nested,
				"required":   []any{"query", 42, nil},
			},
			wantProps:    nested,
			wantRequired: []string{"query"},
		},
		{
			name:         "missing properties and required",
			schema:       map[string]any{"type": "object"},
			wantProps:    nil,
			wantRequired: nil,
		},
		{
			name:         "nil schema",
			schema:       nil,
			wantProps:    nil,
			wantRequired: nil,
		},
		{
			// Anything besides properties/required (additionalProperties,
			// description, ...) is dropped — the Anthropic tool encoding only
			// carries these two. Pins current behavior.
			name: "other schema fields are ignored",
			schema: map[string]any{
				"properties":           nested,
				"additionalProperties": false,
				"description":          "top-level",
			},
			wantProps:    nested,
			wantRequired: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			props, required := schemaProps(tt.schema)
			if !reflect.DeepEqual(props, tt.wantProps) {
				t.Errorf("props = %v, want %v", props, tt.wantProps)
			}
			if !reflect.DeepEqual(required, tt.wantRequired) {
				t.Errorf("required = %v, want %v", required, tt.wantRequired)
			}
		})
	}
}
