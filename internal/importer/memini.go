package importer

import (
	"fmt"

	"github.com/eleboucher/memini/internal/memory"
)

// meminiRecord is memini's own portable shape: a bare array or {"memories": [...]}.
type meminiRecord struct {
	ID         string         `json:"id"`
	Namespace  string         `json:"namespace"`
	Tier       string         `json:"tier"`
	Content    string         `json:"content"`
	Summary    string         `json:"summary"`
	Tags       []string       `json:"tags"`
	Metadata   map[string]any `json:"metadata"`
	Importance float64        `json:"importance"`
	CreatedAt  string         `json:"created_at"`
	UpdatedAt  string         `json:"updated_at"`
	ExpiresAt  string         `json:"expires_at"`
}

func parseMemini(data []byte) ([]Record, error) {
	var arr []meminiRecord
	if err := unmarshalList(data, "memories", &arr); err != nil {
		return nil, fmt.Errorf("import: parse memini: %w", err)
	}
	recs := make([]Record, 0, len(arr))
	for _, r := range arr {
		rec := Record{
			ID:         r.ID,
			Namespace:  r.Namespace,
			Tier:       memory.Tier(r.Tier),
			Content:    r.Content,
			Summary:    r.Summary,
			Tags:       r.Tags,
			Metadata:   r.Metadata,
			Importance: r.Importance,
			CreatedAt:  parseTime(r.CreatedAt),
			UpdatedAt:  parseTime(r.UpdatedAt),
		}
		if t := parseTime(r.ExpiresAt); !t.IsZero() {
			rec.ExpiresAt = &t
		}
		recs = append(recs, rec)
	}
	return recs, nil
}
