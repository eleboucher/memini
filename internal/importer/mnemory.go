package importer

import (
	"fmt"
	"strings"
	"time"

	"github.com/eleboucher/memini/internal/memory"
)

// mnemoryItem maps fpytloun/mnemory's stored memory (mnemory/api/schemas.py).
// mnemory is per-tenant; content may surface as "content" or "memory".
type mnemoryItem struct {
	ID         string         `json:"id"`
	Content    string         `json:"content"`
	Memory     string         `json:"memory"`
	MemoryType string         `json:"memory_type"` // preference|fact|episodic|procedural|context
	Tags       []string       `json:"tags"`
	Importance string         `json:"importance"` // low|medium|high
	TTLDays    *int           `json:"ttl_days"`
	EventDate  string         `json:"event_date"`
	CreatedAt  string         `json:"created_at"`
	Tenant     string         `json:"tenant"`
	Namespace  string         `json:"namespace"`
	Metadata   map[string]any `json:"metadata"`
}

func parseMnemory(data []byte) ([]Record, error) {
	var arr []mnemoryItem
	if err := unmarshalList(data, "memories", &arr); err != nil {
		return nil, fmt.Errorf("import: parse mnemory: %w", err)
	}
	recs := make([]Record, 0, len(arr))
	for _, it := range arr {
		created := parseTime(it.CreatedAt)
		rec := Record{
			ID:         it.ID,
			Namespace:  firstNonEmpty(it.Namespace, it.Tenant),
			Tier:       mnemoryTier(it.MemoryType),
			Content:    firstNonEmpty(it.Content, it.Memory),
			Tags:       it.Tags,
			Metadata:   it.Metadata,
			Importance: importanceWord(it.Importance),
			CreatedAt:  created,
			UpdatedAt:  created,
		}
		// Honor explicit ttl_days only when we know the anchor date; otherwise
		// leave it nil so the tier default applies.
		if it.TTLDays != nil && *it.TTLDays > 0 && !created.IsZero() {
			exp := created.Add(time.Duration(*it.TTLDays) * 24 * time.Hour)
			rec.ExpiresAt = &exp
		}
		recs = append(recs, rec)
	}
	return recs, nil
}

// mnemoryTier maps mnemory's memory_type onto memini tiers.
func mnemoryTier(t string) memory.Tier {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "episodic":
		return memory.TierEpisodic
	case "procedural":
		return memory.TierProcedural
	case "context":
		return memory.TierWorking
	default: // fact, preference, and anything else: durable semantic
		return memory.TierSemantic
	}
}
