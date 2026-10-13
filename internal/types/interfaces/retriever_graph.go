package interfaces

import (
	"context"
	"errors"

	"github.com/Tencent/WeKnora/internal/types"
)

// ErrGraphUnavailable reports that the entity-relationship graph store cannot
// answer queries at all (disabled, unreachable or not configured). It is
// distinct from "this entity has no relations", which is a successful empty
// result, so callers never render an unavailable service as an empty graph.
var ErrGraphUnavailable = errors.New("knowledge graph store is unavailable")

// RetrieveGraphRepository is a repository for retrieving graphs
type RetrieveGraphRepository interface {
	// AddGraph adds a graph to the repository
	AddGraph(ctx context.Context, namespace types.NameSpace, graphs []*types.GraphData) error
	// DelGraph deletes a graph from the repository
	DelGraph(ctx context.Context, namespace []types.NameSpace) error
	// SearchNode searches for nodes in the repository
	SearchNode(ctx context.Context, namespace types.NameSpace, nodes []string) (*types.GraphData, error)
}

// GraphPinger is implemented by graph repositories that can prove their store
// is reachable. SearchNode's empty result and "service down" both look like
// (nil, nil) on the plain interface, so health and status surfaces assert this
// optional capability before claiming a graph is available.
type GraphPinger interface {
	// Ping returns ErrGraphUnavailable (wrapped) when the store cannot serve
	// queries, and nil when it can.
	Ping(ctx context.Context) error
}
