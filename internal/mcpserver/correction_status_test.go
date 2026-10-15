package mcpserver

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/mark3labs/mcp-go/mcp"
)

func TestDocumentStatusBeforeProcessingHasNoTimestamp(t *testing.T) {
	r := describeKnowledgeStatus(&types.Knowledge{ID: "new", ParseStatus: types.ParseStatusPending})
	if r.ProcessedAt != "" || r.IndexStatus != types.ParseStatusPending {
		t.Fatalf("new document status = %+v", r)
	}
}

type statusKnowledgeRepo struct {
	interfaces.KnowledgeRepository
	processingErr error
}

func (r *statusKnowledgeRepo) CountKnowledgeByKnowledgeBaseID(context.Context, uint64, string) (int64, error) { return 2, nil }
func (r *statusKnowledgeRepo) CountKnowledgeByStatus(context.Context, uint64, string, []string) (int64, error) { return 0, r.processingErr }
type statusKnowledgeService struct { interfaces.KnowledgeService; repo interfaces.KnowledgeRepository }
func (s *statusKnowledgeService) GetRepository() interfaces.KnowledgeRepository { return s.repo }
type statusChunkRepo struct { interfaces.ChunkRepository }
func (*statusChunkRepo) CountChunksByKnowledgeBaseID(context.Context, uint64, string) (int64, error) { return 4, nil }
type statusChunkService struct { interfaces.ChunkService }
func (*statusChunkService) GetRepository() interfaces.ChunkRepository { return &statusChunkRepo{} }

func TestKBStatusRequiresEveryCounter(t *testing.T) {
	for _, tc := range []struct{name, missing string; processingErr error; chunks bool}{
		{"processing failure", "processing", errors.New("count failed"), true},
		{"missing chunk service", "chunk", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, _ := enqueueFailureServer(t)
			srv.knowledgeService = &statusKnowledgeService{repo: &statusKnowledgeRepo{processingErr: tc.processingErr}}
			if tc.chunks { srv.chunkService = &statusChunkService{} }
			ep := &types.MCPEndpoint{ID: "ep-1", TenantID: 7}
			var req mcp.CallToolRequest
			req.Params.Arguments = map[string]any{"knowledge_base_id": kbA}
			result, err := srv.handleGetKnowledgeBaseStatus(enqueueFailureContext(ep), req)
			if err != nil || result.IsError { t.Fatalf("status: %v %+v", err, result) }
			payload := result.StructuredContent.(map[string]any)
			report := payload["status"].(knowledgeBaseStatusReport)
			if report.CountsKnown || !strings.Contains(report.Note, tc.missing) { t.Fatalf("incomplete counters = %+v", report) }
		})
	}
}
