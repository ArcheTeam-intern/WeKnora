package mcpserver

import (
	"context"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/mark3labs/mcp-go/mcp"
)

// unknownStatus is the single value every field uses when the server cannot
// determine the truth. The plan is explicit that an indeterminate sub-state
// must be reported as unknown rather than inferred from "the upload worked".
const unknownStatus = "unknown"

// documentStatusReport is the MCP view of one document's processing state. It
// reports the real counters the pipeline maintains and nothing more: WeKnora
// does not track a percentage, so no percentage is invented.
type documentStatusReport struct {
	KnowledgeID string `json:"knowledge_id"`
	Title       string `json:"title,omitempty"`
	// ParseStatus is the pipeline state: pending, processing, finalizing,
	// completed, failed, cancelled or deleting.
	ParseStatus string `json:"parse_status"`
	// ParseStatusKnown is false when the stored value is not one of the states
	// this server recognises, so a client never has to guess.
	ParseStatusKnown bool `json:"parse_status_known"`
	// IndexStatus is derived from parse_status plus the counters below and is
	// always one of: pending, processing, completed, failed, cancelled,
	// deleting, unknown.
	IndexStatus          string `json:"index_status"`
	EnableStatus         string `json:"enable_status,omitempty"`
	SummaryStatus        string `json:"summary_status,omitempty"`
	PendingSubtasksCount int    `json:"pending_subtasks_count"`
	// PendingSubtasksKnown is false when the older row predates the counter.
	PendingSubtasksKnown bool   `json:"pending_subtasks_known"`
	ErrorMessage         string `json:"error_message,omitempty"`
	ProcessedAt          string `json:"processed_at,omitempty"`
	UpdatedAt            string `json:"updated_at,omitempty"`
	FileType             string `json:"file_type,omitempty"`
	FileSize             int64  `json:"file_size,omitempty"`
	Channel              string `json:"channel,omitempty"`
	Note                 string `json:"note,omitempty"`
}

var knownParseStatuses = map[string]struct{}{
	types.ParseStatusPending:    {},
	types.ParseStatusProcessing: {},
	types.ParseStatusFinalizing: {},
	types.ParseStatusCompleted:  {},
	types.ParseStatusFailed:     {},
	types.ParseStatusCancelled:  {},
	types.ParseStatusDeleting:   {},
}

func describeKnowledgeStatus(k *types.Knowledge) documentStatusReport {
	report := documentStatusReport{
		KnowledgeID:          k.ID,
		Title:                k.Title,
		ParseStatus:          k.ParseStatus,
		EnableStatus:         k.EnableStatus,
		SummaryStatus:        k.SummaryStatus,
		PendingSubtasksCount: k.PendingSubtasksCount,
		PendingSubtasksKnown: true,
		ErrorMessage:         k.ErrorMessage,
		FileType:             k.FileType,
		FileSize:             k.FileSize,
		Channel:              k.Channel,
	}
	_, report.ParseStatusKnown = knownParseStatuses[k.ParseStatus]
	switch {
	case !report.ParseStatusKnown:
		report.IndexStatus = unknownStatus
		report.Note = "the stored parse_status is not a state this server knows"
	case k.ParseStatus == types.ParseStatusFinalizing:
		report.IndexStatus = types.ParseStatusProcessing
		report.Note = "the text index is written; enrichment subtasks (summary, questions, graph) are still running"
	case k.ParseStatus == types.ParseStatusCompleted && k.PendingSubtasksCount > 0:
		// Defensive: a completed row with pending subtasks is inconsistent, and
		// reporting "completed" would overstate what is available.
		report.IndexStatus = types.ParseStatusFinalizing
		report.Note = "parse_status is completed but subtasks are still pending"
	default:
		report.IndexStatus = k.ParseStatus
	}
	if !k.ProcessedAt.IsZero() {
		report.ProcessedAt = k.ProcessedAt.Format("2006-01-02T15:04:05Z07:00")
	}
	if !k.UpdatedAt.IsZero() {
		report.UpdatedAt = k.UpdatedAt.Format("2006-01-02T15:04:05Z07:00")
	}
	// "pending" in the column covers both "queued" and "never started", which
	// this server cannot distinguish; say so instead of implying progress.
	if k.ParseStatus == types.ParseStatusPending {
		report.Note = "queued or not started yet; the server cannot tell which"
	}
	return report
}

func getDocumentStatusTool() mcp.Tool {
	return mcp.NewTool(types.MCPEndpointToolGetDocumentStatus,
		mcp.WithDescription("Report one document's real processing state: parse status, pending enrichment "+
			"subtasks, failure message and timestamps. Use it before retrying a search that assumed the "+
			"document was already indexed."),
		mcp.WithString("knowledge_id", mcp.Required(), mcp.Description("Document id")),
		mcp.WithReadOnlyHintAnnotation(true),
	)
}

func (s *Server) handleGetDocumentStatus(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ep, err := endpointFromContext(ctx)
	if err != nil {
		return errorResult(&ToolError{Code: CodeUnauthorized, Message: "unauthorized"}), nil
	}
	knowledgeID, err := req.RequireString("knowledge_id")
	if err != nil {
		return errorResult(invalidArgument("knowledge_id is required")), nil
	}
	k, kb, err := s.knowledgeInScope(ctx, ep, knowledgeID)
	if err != nil {
		return errorResult(err), nil
	}
	ctx, err = s.scopedKBContext(ctx, kb, types.OrgRoleViewer)
	if err != nil {
		return errorResult(err), nil
	}
	return jsonResult(map[string]any{
		"knowledge_base_id": kb.ID,
		"status":            describeKnowledgeStatus(k),
	})
}

// knowledgeBaseStatusReport is the MCP view of one knowledge base's generation
// state. Each capability is reported separately and honestly: a knowledge base
// can have a working vector index while its wiki is still being generated.
type knowledgeBaseStatusReport struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	Type string `json:"type,omitempty"`
	// Capabilities are the configured switches on the row, i.e. what was asked
	// for, not proof that generated artefacts exist yet.
	Capabilities map[string]bool `json:"configured_capabilities"`
	// Documents / Chunks are real counters when CountsKnown is true.
	Documents    int64 `json:"documents"`
	Chunks       int64 `json:"chunks"`
	Processing   int64 `json:"processing_documents"`
	CountsKnown  bool  `json:"counts_known"`
	IsProcessing bool  `json:"is_processing"`
	// Wiki is present only when the wiki is configured, and reports the real
	// pending-queue depth and running flag exposed by the wiki service.
	Wiki *wikiStatusReport `json:"wiki,omitempty"`
	// Graph is present only when graph extraction is configured.
	Graph *graphStatusReport `json:"graph,omitempty"`
	Note  string             `json:"note,omitempty"`
}

type wikiStatusReport struct {
	Enabled       bool             `json:"enabled"`
	Available     string           `json:"available"`
	AvailableErr  string           `json:"available_error,omitempty"`
	TotalPages    int64            `json:"total_pages"`
	PagesByType   map[string]int64 `json:"pages_by_type,omitempty"`
	TotalLinks    int64            `json:"total_links"`
	OrphanCount   int64            `json:"orphan_pages"`
	PendingTasks  int64            `json:"pending_tasks"`
	PendingIssues int64            `json:"pending_issues"`
	IsActive      bool             `json:"is_active"`
}

type graphStatusReport struct {
	Enabled       bool     `json:"enabled"`
	Available     string   `json:"available"`
	AvailableErr  string   `json:"available_error,omitempty"`
	EntityTypes   []string `json:"entity_types,omitempty"`
	RelationTypes []string `json:"relation_types,omitempty"`
	Note          string   `json:"note,omitempty"`
}

func getKnowledgeBaseStatusTool() mcp.Tool {
	return mcp.NewTool(types.MCPEndpointToolGetKBStatus,
		mcp.WithDescription("Report a knowledge base's generation state: document and chunk counts, the number "+
			"of documents still being processed, wiki page totals, the pending wiki task depth, whether wiki "+
			"ingestion is running, and whether the entity-relationship graph is enabled and reachable. Use it "+
			"to decide whether generated content exists yet instead of assuming it does."),
		mcp.WithString("knowledge_base_id", mcp.Required(), mcp.Description("Knowledge base id or exact name")),
		mcp.WithReadOnlyHintAnnotation(true),
	)
}

func (s *Server) handleGetKnowledgeBaseStatus(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ep, err := endpointFromContext(ctx)
	if err != nil {
		return errorResult(&ToolError{Code: CodeUnauthorized, Message: "unauthorized"}), nil
	}
	selector, err := req.RequireString("knowledge_base_id")
	if err != nil {
		return errorResult(invalidArgument("knowledge_base_id is required")), nil
	}
	kbs, err := s.selectKnowledgeBases(ctx, ep, []string{selector})
	if err != nil {
		return errorResult(err), nil
	}
	kb := kbs[0]
	ctx, err = s.scopedKBContext(ctx, kb, types.OrgRoleViewer)
	if err != nil {
		return errorResult(err), nil
	}

	report := knowledgeBaseStatusReport{
		ID:   kb.ID,
		Name: kb.Name,
		Type: kb.Type,
		Capabilities: map[string]bool{
			"vector":  kb.IsVectorEnabled(),
			"keyword": kb.IsKeywordEnabled(),
			"wiki":    kb.IsWikiEnabled(),
			"graph":   kb.IsGraphEnabled(),
			"faq":     kb.Type == types.KnowledgeBaseTypeFAQ,
		},
	}

	// Counts come from the same counters the admin API shows. They are read from
	// the knowledge base record rather than counted here, because the count
	// methods live on the knowledge REPOSITORY (not the service) and the service
	// already fills these fields for exactly this purpose.
	//
	// A nil counter is reported as unknown rather than as zero: zero documents
	// and "could not count" must not look the same to an agent.
	if kb.KnowledgeCount >= 0 && kb.ChunkCount >= 0 {
		report.Documents = kb.KnowledgeCount
		report.Chunks = kb.ChunkCount
		report.Processing = kb.ProcessingCount
		report.CountsKnown = true
	} else {
		report.Note = "document counters are unavailable; treat the counts as unknown"
	}
	report.IsProcessing = kb.IsProcessing || report.Processing > 0

	if kb.IsWikiEnabled() {
		wiki := &wikiStatusReport{Enabled: true, Available: unknownStatus}
		if s.wikiService != nil {
			stats, statsErr := s.wikiService.GetStats(ctx, kb.ID)
			switch {
			case statsErr != nil:
				wiki.Available = "unavailable"
				wiki.AvailableErr = statsErr.Error()
			case stats == nil:
				wiki.Available = unknownStatus
				wiki.AvailableErr = "the wiki service returned no statistics"
			default:
				wiki.Available = "available"
				wiki.TotalPages = stats.TotalPages
				wiki.PagesByType = stats.PagesByType
				wiki.TotalLinks = stats.TotalLinks
				wiki.OrphanCount = stats.OrphanCount
				wiki.PendingTasks = stats.PendingTasks
				wiki.PendingIssues = stats.PendingIssues
				wiki.IsActive = stats.IsActive
			}
		} else {
			wiki.AvailableErr = "the wiki service is not wired into this server"
		}
		report.Wiki = wiki
	}

	if kb.IsGraphEnabled() {
		graph := &graphStatusReport{Enabled: true}
		graph.Available, graph.AvailableErr = graphServiceStatus(ctx, s.graphRepo)
		if kb.ExtractConfig != nil {
			// Entity and relation types are structured objects; the report lists
			// their names so a reader sees what the extraction is configured for.
			for _, node := range kb.ExtractConfig.Nodes {
				if node != nil && strings.TrimSpace(node.Name) != "" {
					graph.EntityTypes = append(graph.EntityTypes, node.Name)
				}
			}
			for _, relation := range kb.ExtractConfig.Relations {
				if relation != nil && strings.TrimSpace(relation.Type) != "" {
					graph.RelationTypes = append(graph.RelationTypes, relation.Type)
				}
			}
		}
		if len(graph.EntityTypes) == 0 && len(graph.RelationTypes) == 0 {
			graph.Note = "graph extraction is switched on but no entity or relation types are configured, " +
				"so no relations can be extracted"
		} else if graph.Available == "available" {
			graph.Note = "the graph store is reachable; relation counts are per query, not stored here"
		}
		report.Graph = graph
	}

	return jsonResult(map[string]any{
		"knowledge_base_id": kb.ID,
		"status":            report,
	})
}
