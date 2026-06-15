package importer

import (
	"fmt"

	"github.com/eleboucher/memini/internal/memory"
)

// mem0Item maps mem0ai/mem0's MemoryItem (mem0/configs/base.py). get_all() wraps
// them under "results"; metadata typically carries the session scope
// (user_id/agent_id/run_id) and categories.
type mem0Item struct {
	ID        string         `json:"id"`
	Memory    string         `json:"memory"`
	Metadata  map[string]any `json:"metadata"`
	CreatedAt string         `json:"created_at"`
	UpdatedAt string         `json:"updated_at"`
	// Session scope is sometimes promoted to top-level fields.
	UserID     string   `json:"user_id"`
	AgentID    string   `json:"agent_id"`
	RunID      string   `json:"run_id"`
	Categories []string `json:"categories"`
}

// mem0Tier maps mem0's MemoryType (mem0/configs/enums.py), carried in
// metadata["memory_type"], onto memini tiers. mem0's default extraction is
// untyped and produces durable facts, so an absent/unknown type is semantic.
func mem0Tier(t string) memory.Tier {
	switch t {
	case "procedural_memory":
		return memory.TierProcedural
	case "episodic_memory":
		return memory.TierEpisodic
	default: // "semantic_memory" and mem0's default untyped extractions
		return memory.TierSemantic
	}
}

func parseMem0(data []byte) ([]Record, error) {
	var arr []mem0Item
	if err := unmarshalList(data, "results", &arr); err != nil {
		return nil, fmt.Errorf("import: parse mem0: %w", err)
	}
	recs := make([]Record, 0, len(arr))
	for _, it := range arr {
		ns := firstNonEmpty(it.UserID, it.AgentID, it.RunID,
			metaString(it.Metadata, "user_id"),
			metaString(it.Metadata, "agent_id"),
			metaString(it.Metadata, "run_id"))
		tags := it.Categories
		if len(tags) == 0 {
			tags = metaStrings(it.Metadata, "categories")
		}
		recs = append(recs, Record{
			ID:         it.ID,
			Namespace:  ns,
			Tier:       mem0Tier(metaString(it.Metadata, "memory_type")),
			Content:    it.Memory,
			Tags:       tags,
			Metadata:   it.Metadata,
			Importance: 0.5,
			CreatedAt:  parseTime(it.CreatedAt),
			UpdatedAt:  parseTime(it.UpdatedAt),
		})
	}
	return recs, nil
}
