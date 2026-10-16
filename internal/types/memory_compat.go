package types

import (
	"database/sql/driver"
	"encoding/json"
)

// Long-term memory was retired from this build (the service, the repositories,
// the recall/affinity pipeline plugins, the search tool and the API are gone).
// Two persisted shapes stay here on purpose, because existing rows still carry
// them and a code change must not make stored data unreadable:
//
//   - messages.used_memories (UsedMemories)
//   - tenants.memory_config  (MemoryConfig)
//
// They are data shapes only: nothing reads or interprets them any more, and no
// new value is ever written. Dropping the columns is a separate, auditable data
// migration, out of scope for the code pruning plan.

// UsedMemory records one memory that influenced an answer. The type name is
// kept because the column it feeds is named after it.
type UsedMemory struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Content string `json:"content"`
}

// UsedMemories is the persisted per-message list.
type UsedMemories []UsedMemory

func (u UsedMemories) Value() (driver.Value, error) {
	if u == nil {
		return json.Marshal([]UsedMemory{})
	}
	return json.Marshal(u)
}

func (u *UsedMemories) Scan(value interface{}) error {
	if value == nil {
		*u = make(UsedMemories, 0)
		return nil
	}
	var b []byte
	switch v := value.(type) {
	case []byte:
		b = v
	case string:
		b = []byte(v)
	default:
		*u = make(UsedMemories, 0)
		return nil
	}
	if len(b) == 0 {
		*u = make(UsedMemories, 0)
		return nil
	}
	return json.Unmarshal(b, u)
}

// MemoryConfig is the workspace-level settings row written by builds that had
// the feature. Retained so an upgrade never fails to load an existing tenant
// row; every field keeps its stored meaning, and the product no longer acts on
// any of them.
type MemoryConfig struct {
	Enabled                   bool   `json:"enabled"`
	WriteMode                 string `json:"write_mode"`
	ExtractModelID            string `json:"extract_model_id"`
	MaxItems                  int    `json:"max_items"`
	ExtractDelaySeconds       int    `json:"extract_delay_seconds"`
	ExtractMinIntervalSeconds int    `json:"extract_min_interval_seconds"`
	ExtractInstructions       string `json:"extract_instructions"`
	InterestThreshold         int    `json:"interest_threshold"`
	EmbeddingModelID          string `json:"embedding_model_id"`
	VectorRecall              *bool  `json:"vector_recall"`
	RetrievalConditioning     *bool  `json:"retrieval_conditioning"`
}

func (c MemoryConfig) Value() (driver.Value, error) { return json.Marshal(c) }

func (c *MemoryConfig) Scan(value interface{}) error {
	if value == nil {
		return nil
	}
	var b []byte
	switch v := value.(type) {
	case []byte:
		b = v
	case string:
		b = []byte(v)
	default:
		return nil
	}
	if len(b) == 0 {
		return nil
	}
	return json.Unmarshal(b, c)
}
