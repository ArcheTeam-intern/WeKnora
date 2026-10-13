package types

// ChunkContext represents chunk content with surrounding context
type ChunkContext struct {
	ChunkID     string `json:"chunk_id"`
	Content     string `json:"content"`
	PrevContent string `json:"prev_content,omitempty"` // Previous chunk content for context
	NextContent string `json:"next_content,omitempty"` // Next chunk content for context
}

// PromptTemplateStructured represents the prompt template structured
type PromptTemplateStructured struct {
	Description string      `json:"description"`
	Tags        []string    `json:"tags"`
	Examples    []GraphData `json:"examples"`
}

type GraphNode struct {
	Name       string   `json:"name,omitempty"`
	Chunks     []string `json:"chunks,omitempty"`
	Attributes []string `json:"attributes,omitempty"`
	// Retrieval identities are local to the graph query, not extraction configuration.
	ID          string `json:"-" yaml:"-"`
	KnowledgeID string `json:"-" yaml:"-"`
}

// GraphRelation represents the relation of the graph
type GraphRelation struct {
	Node1 string `json:"node1,omitempty"`
	Node2 string `json:"node2,omitempty"`
	Type  string `json:"type,omitempty"`
	// Names are for display; these identities link to the actual document instances.
	ID       string `json:"-" yaml:"-"`
	SourceID string `json:"-" yaml:"-"`
	TargetID string `json:"-" yaml:"-"`
	// Per-relation provenance, filled in by the extraction service (not by the
	// extraction model) and persisted on the relationship so a query can cite the
	// exact document chunks that produced this relation. A relation without these
	// (pre-provenance rows) reads as unknown rather than borrowing another
	// relation's evidence.
	KnowledgeID string   `json:"-" yaml:"-"`
	ChunkIDs    []string `json:"-" yaml:"-"`
}

type GraphData struct {
	Text     string           `json:"text,omitempty"`
	Node     []*GraphNode     `json:"node,omitempty"`
	Relation []*GraphRelation `json:"relation,omitempty"`
	// RelationTruncated reports that the graph store hit its row cap while
	// reading this result, so Relation is a subset of the matched relations and
	// not the entity's whole neighbourhood. It is an in-process signal (never
	// persisted, never model-output) so callers can surface truncation instead of
	// treating a capped list as complete.
	RelationTruncated bool `json:"-" yaml:"-"`
}

// NameSpace represents the name space of the knowledge base and knowledge
type NameSpace struct {
	KnowledgeBase string `json:"knowledge_base"`
	Knowledge     string `json:"knowledge"`
}

// Labels returns the labels of the name space
func (n NameSpace) Labels() []string {
	res := make([]string, 0)
	if n.KnowledgeBase != "" {
		res = append(res, n.KnowledgeBase)
	}
	if n.Knowledge != "" {
		res = append(res, n.Knowledge)
	}
	return res
}
