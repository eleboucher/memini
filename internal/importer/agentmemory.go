package importer

import (
	"fmt"

	"github.com/eleboucher/memini/internal/memory"
)

// agentMemoryItem maps rohitg00/agentmemory's exported Memory record (see
// src/types.ts:Memory). The export bundle wraps them under "memories".
type agentMemoryItem struct {
	ID          string   `json:"id"`
	CreatedAt   string   `json:"createdAt"`
	UpdatedAt   string   `json:"updatedAt"`
	Type        string   `json:"type"` // pattern|preference|architecture|bug|workflow|fact
	Title       string   `json:"title"`
	Content     string   `json:"content"`
	Concepts    []string `json:"concepts"`
	Strength    float64  `json:"strength"`
	Project     string   `json:"project"`
	ForgetAfter string   `json:"forgetAfter"`
}

func parseAgentMemory(data []byte) ([]Record, error) {
	var arr []agentMemoryItem
	if err := unmarshalList(data, "memories", &arr); err != nil {
		return nil, fmt.Errorf("import: parse agentmemory: %w", err)
	}
	recs := make([]Record, 0, len(arr))
	for _, m := range arr {
		rec := Record{
			ID:         m.ID,
			Namespace:  m.Project,
			Tier:       agentMemoryTier(m.Type),
			Content:    m.Content,
			Summary:    m.Title,
			Tags:       m.Concepts,
			Importance: m.Strength,
			CreatedAt:  parseTime(m.CreatedAt),
			UpdatedAt:  parseTime(m.UpdatedAt),
		}
		if t := parseTime(m.ForgetAfter); !t.IsZero() {
			rec.ExpiresAt = &t
		}
		recs = append(recs, rec)
	}
	return recs, nil
}

// agentMemoryTier maps agentmemory's memory types onto memini tiers: how-to
// knowledge is procedural, everything else is durable semantic fact.
func agentMemoryTier(t string) memory.Tier {
	switch t {
	case "pattern", "architecture", "workflow":
		return memory.TierProcedural
	default: // preference, fact, bug
		return memory.TierSemantic
	}
}
