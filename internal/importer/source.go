package importer

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Source identifies the export format of the data being imported.
type Source string

const (
	// SourceMemini is memini's own export shape (a JSON array of Records).
	SourceMemini Source = "memini"
	// SourceAgentMemory is rohitg00/agentmemory's export bundle.
	SourceAgentMemory Source = "agentmemory"
	// SourceMem0 is mem0ai/mem0's get_all / export output.
	SourceMem0 Source = "mem0"
	// SourceMnemory is fpytloun/mnemory's export output.
	SourceMnemory Source = "mnemory"
	// SourceClaudeCode is a Claude Code session transcript (JSONL), reconstructed
	// into per-exchange episodic memories.
	SourceClaudeCode Source = "claude-code"
)

// Sources lists the supported import sources, sorted.
func Sources() []string {
	out := []string{
		string(SourceMemini), string(SourceAgentMemory),
		string(SourceMem0), string(SourceMnemory), string(SourceClaudeCode),
	}
	sort.Strings(out)
	return out
}

// Parse converts a source export (raw JSON) into portable Records.
func Parse(src Source, data []byte) ([]Record, error) {
	switch src {
	case SourceMemini:
		return parseMemini(data)
	case SourceAgentMemory:
		return parseAgentMemory(data)
	case SourceMem0:
		return parseMem0(data)
	case SourceMnemory:
		return parseMnemory(data)
	case SourceClaudeCode:
		return parseClaudeCode(data)
	default:
		return nil, fmt.Errorf("import: unknown source %q (want one of %s)",
			src, strings.Join(Sources(), ", "))
	}
}

// parseTime parses the common timestamp shapes (RFC3339 / RFC3339-nano /
// "2006-01-02 15:04:05"); a blank or unparseable value yields the zero time so
// the importer falls back to import-time.
func parseTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// importanceWord maps coarse "low|medium|high" levels (mnemory/agentmemory) to
// memini's [0,1] importance scale.
func importanceWord(s string) float64 {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "high", "critical":
		return 0.9
	case "medium", "normal", "":
		return 0.5
	case "low", "minor":
		return 0.2
	default:
		return 0.5
	}
}

// unmarshalList decodes either a bare JSON array or an object that wraps the
// array under key (e.g. {"results": [...]}) into dst.
func unmarshalList(data []byte, key string, dst any) error {
	wrapper := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &wrapper); err == nil {
		if raw, ok := wrapper[key]; ok {
			return json.Unmarshal(raw, dst)
		}
	}
	return json.Unmarshal(data, dst)
}

// firstNonEmpty returns the first non-empty, trimmed string.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// metaString reads a string field from a metadata map.
func metaString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// metaStrings reads a []string field from a metadata map (JSON arrays decode to
// []any).
func metaStrings(m map[string]any, key string) []string {
	if m == nil {
		return nil
	}
	raw, ok := m[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
