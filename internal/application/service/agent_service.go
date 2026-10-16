package service

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/Tencent/WeKnora/internal/agent"
	"github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/models/rerank"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	secutils "github.com/Tencent/WeKnora/internal/utils"
	"gorm.io/gorm"
)

const MAX_ITERATIONS = 100 // Max iterations for agent execution

// dedupStrings removes duplicate strings while preserving the first occurrence order.
func dedupStrings(in []string) []string {
	if len(in) == 0 {
		return in
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// withoutString returns the slice with every occurrence of drop removed,
// preserving order.
func withoutString(in []string, drop string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s != drop {
			out = append(out, s)
		}
	}
	return out
}

// agentHasKnowledgeScope reports whether the agent has any KB retrieval scope for
// this turn. Tag-only @mentions populate SearchTargets (with TagIDs) but leave
// KnowledgeBases / KnowledgeIDs empty — those must still count as in-scope.
func agentHasKnowledgeScope(config *types.AgentConfig) bool {
	if config == nil {
		return false
	}
	return types.HasKnowledgeRetrievalScope(
		config.SearchTargets,
		config.KnowledgeBases,
		config.KnowledgeIDs,
	)
}

// knowledgeBaseScopesForPrompt returns the KB IDs to show in runtime_context
// metadata, together with the tenant each KB should be queried under.
//
// The tenant map always comes from SearchTargets, which buildSearchTargets has
// already resolved (and authorized) per KB: a directly shared KB carries its
// source tenant there. KnowledgeBases alone cannot tell own from shared KBs —
// both KBSelectionMode="all" and an @mention put shared KB IDs into it.
// KBs missing from the map fall back to the caller's tenant.
func knowledgeBaseScopesForPrompt(config *types.AgentConfig) ([]string, map[string]uint64) {
	if config == nil {
		return nil, nil
	}
	kbTenantMap := config.SearchTargets.GetKBTenantMap()
	if len(config.KnowledgeBases) > 0 {
		return config.KnowledgeBases, kbTenantMap
	}
	return config.SearchTargets.GetAllKnowledgeBaseIDs(), kbTenantMap
}

// agentService implements agent-related business logic
type agentService struct {
	userRepo             interfaces.UserRepository
	graphRepo            interfaces.RetrieveGraphRepository
	cfg                  *config.Config
	modelService         interfaces.ModelService
	eventBus             *event.EventBus
	db                   *gorm.DB
	knowledgeBaseService interfaces.KnowledgeBaseService
	knowledgeService     interfaces.KnowledgeService
	fileService          interfaces.FileService
	chunkService         interfaces.ChunkService
	duckdb               *sql.DB
	wikiPageService      interfaces.WikiPageService
	tenantService        interfaces.TenantService
	messageService       interfaces.MessageService
	storageResolver      interfaces.StorageBackendResolver
}

// NewAgentService creates a new agent service
func NewAgentService(
	cfg *config.Config,
	modelService interfaces.ModelService,
	knowledgeBaseService interfaces.KnowledgeBaseService,
	knowledgeService interfaces.KnowledgeService,
	fileService interfaces.FileService,
	chunkService interfaces.ChunkService,
	eventBus *event.EventBus,
	db *gorm.DB,
	duckdb *sql.DB,
	wikiPageService interfaces.WikiPageService,
	tenantService interfaces.TenantService,
	messageService interfaces.MessageService,
	storageResolver interfaces.StorageBackendResolver,
	userRepo interfaces.UserRepository,
	graphRepo interfaces.RetrieveGraphRepository,
) interfaces.AgentService {
	svc := &agentService{
		userRepo:             userRepo,
		graphRepo:            graphRepo,
		cfg:                  cfg,
		modelService:         modelService,
		knowledgeBaseService: knowledgeBaseService,
		knowledgeService:     knowledgeService,
		fileService:          fileService,
		chunkService:         chunkService,
		eventBus:             eventBus,
		db:                   db,
		duckdb:               duckdb,
		wikiPageService:      wikiPageService,
		tenantService:        tenantService,
		messageService:       messageService,
		storageResolver:      storageResolver,
	}
	return svc
}

// CreateAgentEngine creates an agent engine with the given configuration and EventBus.
// History is loaded once per turn by the caller (see service.LoadAgentHistory)
// and handed to AgentEngine.Execute as llmContext; the engine is stateless across turns.
func (s *agentService) CreateAgentEngine(
	ctx context.Context,
	config *types.AgentConfig,
	chatModel chat.Chat,
	rerankModel rerank.Reranker,
	eventBus *event.EventBus,
	sessionID string,
) (interfaces.AgentEngine, error) {
	logger.Infof(ctx, "Creating agent engine with custom EventBus")

	// 1. Validate config
	if err := s.ValidateConfig(config); err != nil {
		return nil, fmt.Errorf("invalid agent config: %w", err)
	}
	if chatModel == nil {
		return nil, fmt.Errorf("chat model is nil after initialization")
	}

	// 2. Build tool registry
	toolRegistry := tools.NewToolRegistry()
	if config.MaxToolOutputChars > 0 {
		toolRegistry.SetMaxToolOutputSize(config.MaxToolOutputChars)
	}
	if err := s.registerTools(ctx, toolRegistry, config, rerankModel, chatModel, sessionID); err != nil {
		return nil, fmt.Errorf("failed to register tools: %w", err)
	}

	// 3. Resolve knowledge base and selected document metadata
	kbInfos, selectedDocs := s.resolveKBAndDocInfos(ctx, config)

	// 4. Resolve system prompt template
	systemPromptTemplate := ""
	if config.UseCustomSystemPrompt || config.SystemPrompt != "" {
		systemPromptTemplate = config.ResolveSystemPrompt()
	}

	// 5. Create engine
	engine := agent.NewAgentEngine(
		config, chatModel, toolRegistry, eventBus,
		kbInfos, selectedDocs, sessionID,
		systemPromptTemplate,
	)
	engine.SetAppConfig(s.cfg)
	engine.SetQuestionOrigin(s.resolveQuestionOriginInfo(ctx, config.QuestionOrigin, config.SearchTargets, kbInfos))

	// Non-vision chat models use the configured VLM to describe tool images.
	// Vision chat models receive the original images after the tool replies.
	if config.VLMModelID != "" {
		if vlmModel, err := s.modelService.GetVLMModel(ctx, config.VLMModelID); err == nil {
			engine.SetImageDescriber(func(ctx context.Context, imgBytes []byte, prompt string) (string, error) {
				return vlmModel.Predict(ctx, [][]byte{imgBytes}, prompt)
			})
			logger.Infof(ctx, "VLM image describer set for tool result analysis (model: %s)", config.VLMModelID)
		} else {
			logger.Warnf(ctx, "Failed to load VLM model %s for tool image fallback: %v", config.VLMModelID, err)
		}
	}

	toolRegistry.BindSession(sessionID)

	return engine, nil
}

// resolveKBAndDocInfos loads knowledge base metadata and selected document info for prompt.
func (s *agentService) resolveKBAndDocInfos(
	ctx context.Context,
	config *types.AgentConfig,
) ([]*agent.KnowledgeBaseInfo, []*agent.SelectedDocumentInfo) {
	kbIDs, kbTenantMap := knowledgeBaseScopesForPrompt(config)
	kbInfos, err := s.getKnowledgeBaseInfos(ctx, kbIDs, kbTenantMap)
	if err != nil {
		logger.Warnf(ctx, "Failed to get knowledge base details, using IDs only: %v", err)
		kbInfos = make([]*agent.KnowledgeBaseInfo, 0, len(kbIDs))
		for _, kbID := range kbIDs {
			kbInfos = append(kbInfos, &agent.KnowledgeBaseInfo{
				ID:          kbID,
				Name:        kbID,
				Description: "",
				DocCount:    0,
			})
		}
	}

	selectedDocs, err := s.getSelectedDocumentInfos(ctx, config.KnowledgeIDs)
	if err != nil {
		logger.Warnf(ctx, "Failed to get selected document details: %v", err)
		selectedDocs = []*agent.SelectedDocumentInfo{}
	}

	return kbInfos, selectedDocs
}

// registerTools registers tools based on the agent configuration
func (s *agentService) registerTools(
	ctx context.Context,
	registry *tools.ToolRegistry,
	config *types.AgentConfig,
	rerankModel rerank.Reranker,
	chatModel chat.Chat,
	sessionID string,
) error {
	// Source of truth policy:
	//   - `config.AllowedTools` is the explicit, user-editable whitelist —
	//     populated by the agent-type preset on create and freely editable
	//     afterwards.
	//   - We never silently *inject* tools the user didn't pick.
	//   - We still *filter out* tools whose capability prerequisites are missing
	//     (no KB in scope, no Wiki-capable KB, etc.) so the LLM can't call tools
	//     that would error at runtime.
	//   - Legacy agents without AllowedTools fall back to DefaultAllowedTools().
	var allowedTools []string
	if len(config.AllowedTools) > 0 {
		// Retired retrieval tool names in stored configs map onto their
		// successors here, so no data migration is needed.
		allowedTools = tools.NormalizeAllowedTools(config.AllowedTools)
		logger.Infof(ctx, "Using custom allowed tools from config: %v", allowedTools)
	} else {
		allowedTools = tools.DefaultAllowedTools()
		logger.Infof(ctx, "Using default allowed tools: %v", allowedTools)
	}
	if config.SharedAgentReadOnly {
		allowedTools = withoutWikiWriteTools(allowedTools)
	}
	// Knowledge-core profile: whatever the stored agent configuration asks for,
	// the reachable tool set is the knowledge surface only. Retired
	// capabilities (web search/fetch, memory, conversation-history search,
	// shell, sandbox files, skills, browser) are dropped here, so an old custom
	// agent cannot re-enable them by keeping them in its allowlist. An agent
	// left with nothing is refused below rather than silently granted the
	// defaults.
	if s.cfg != nil && s.cfg.IsKnowledgeCore() {
		before := len(allowedTools)
		allowedTools = tools.FilterToolsForCoreProfile(allowedTools)
		logger.Infof(ctx, "knowledge-core profile: agent tools filtered from %d to %d entries",
			before, len(allowedTools))
		if len(allowedTools) == 0 {
			return fmt.Errorf("this agent's tools are all retired by the knowledge-core profile; " +
				"update the agent to use retrieval, reading, wiki or graph tools")
		}
	}

	// ---- Capability detection from SearchTargets ----
	var hasVectorKB bool
	var hasGraphKB bool
	var wikiKBIDs []string
	wikiRoutes := tools.NewWikiRouteResolver()
	for _, target := range config.SearchTargets {
		if target == nil || target.KnowledgeBaseID == "" {
			continue
		}
		kb, err := s.knowledgeBaseService.GetKnowledgeBaseByIDOnly(ctx, target.KnowledgeBaseID)
		if err != nil {
			continue
		}
		if kb.IsVectorEnabled() || kb.IsKeywordEnabled() {
			hasVectorKB = true
		}
		if kb.IsGraphEnabled() {
			hasGraphKB = true
		}
		if kb.IsWikiEnabled() {
			wikiKBIDs = append(wikiKBIDs, kb.ID)
		}
	}
	wikiKBIDs = dedupStrings(wikiKBIDs)
	wikiScopes := tools.NewWikiScopesFromSearchTargets(config.SearchTargets, wikiKBIDs)
	// Narrow to the KBs that survived scope resolution. Build a fresh slice
	// rather than truncating in place, so the argument passed above can never
	// be overwritten through a shared backing array.
	scopedWikiKBIDs := make([]string, 0, len(wikiScopes))
	for _, scope := range wikiScopes {
		scopedWikiKBIDs = append(scopedWikiKBIDs, scope.KnowledgeBaseID)
	}
	wikiKBIDs = scopedWikiKBIDs
	hasWikiKB := len(wikiKBIDs) > 0
	// Search targets only need read access. Wiki mutations stay on the KBs the
	// caller may edit, so a viewer share (or the caller's own agent pointed at
	// one) cannot become a write path into another workspace's wiki.
	writableWikiKBIDs := intersectStrings(wikiKBIDs, config.WritableKBIDs)
	if len(writableWikiKBIDs) == 0 {
		allowedTools = withoutWikiWriteTools(allowedTools)
	}

	// Filter out knowledge base tools if no knowledge scope is configured for this turn.
	hasKnowledge := agentHasKnowledgeScope(config)
	if !hasKnowledge {
		filteredTools := make([]string, 0)
		kbTools := map[string]bool{
			tools.ToolSearchKnowledge:     true,
			tools.ToolReadDocument:        true,
			tools.ToolListDocuments:       true,
			tools.ToolQueryKnowledgeGraph: true,
			tools.ToolDatabaseQuery:       true,
			tools.ToolDataAnalysis:        true,
			tools.ToolDataSchema:          true,
			// Wiki tools also require at least one KB in scope.
			tools.ToolWikiReadPage:    true,
			tools.ToolWikiSearch:      true,
			tools.ToolWikiFlagIssue:   true,
			tools.ToolWikiWritePage:   true,
			tools.ToolWikiReplaceText: true,
			tools.ToolWikiRenamePage:  true,
			tools.ToolWikiDeletePage:  true,
			tools.ToolWikiReadIssue:   true,
			tools.ToolWikiUpdateIssue: true,
		}

		// No knowledge targets at all: also disable todo_write (not useful for
		// simple chat).
		kbTools[tools.ToolTodoWrite] = true

		for _, toolName := range allowedTools {
			if !kbTools[toolName] {
				filteredTools = append(filteredTools, toolName)
			}
		}
		allowedTools = filteredTools
		logger.Infof(ctx, "Pure Agent Mode: Knowledge base tools filtered out, remaining: %v", allowedTools)
	}

	// Public web search is retired: both tools are dropped even when a stored
	// allowlist or a preset still names them, and nothing re-adds them.
	allowedTools = withoutString(allowedTools, tools.ToolWebSearch)
	allowedTools = withoutString(allowedTools, tools.ToolWebFetch)

	// Long-term memory is retired: the tool is dropped even when a stored
	// allowlist still names it — a preset, an API caller, or a config saved
	// while memory was on — and nothing re-adds it.
	allowedTools = withoutString(allowedTools, tools.ToolSearchMemory)

	// Tool capability sets — used by the hard safety nets below to drop tools
	// whose runtime prerequisite (a matching KB surface) is missing.
	//
	// NOTE: ragToolSet must stay in sync with frontend `knowledgeBaseTools`
	// in AgentEditorModal.vue. These are *all* tools that retrieve/inspect
	// content from RAG-style knowledge bases.
	ragToolSet := map[string]bool{
		tools.ToolSearchKnowledge:     true,
		tools.ToolQueryKnowledgeGraph: true,
		tools.ToolDatabaseQuery:       true,
	}
	// Document readers work on stored chunks, which every KB writes whatever
	// its indexing strategy, so a wiki-only scope keeps them: they are how a
	// wiki reader checks the source documents a page cites.
	documentToolSet := map[string]bool{
		tools.ToolReadDocument:  true,
		tools.ToolListDocuments: true,
	}
	allWikiToolSet := map[string]bool{
		tools.ToolWikiReadPage:    true,
		tools.ToolWikiSearch:      true,
		tools.ToolWikiFlagIssue:   true,
		tools.ToolWikiWritePage:   true,
		tools.ToolWikiReplaceText: true,
		tools.ToolWikiRenamePage:  true,
		tools.ToolWikiDeletePage:  true,
		tools.ToolWikiReadIssue:   true,
		tools.ToolWikiUpdateIssue: true,
	}

	// Hard safety nets: drop tools whose runtime prerequisite is missing.
	// This guards against stale configs where e.g. the user ticked wiki tools
	// earlier but later swapped in a non-wiki KB (or vice versa for RAG).
	if !hasWikiKB {
		filtered := make([]string, 0, len(allowedTools))
		dropped := make([]string, 0)
		for _, t := range allowedTools {
			if allWikiToolSet[t] {
				dropped = append(dropped, t)
				continue
			}
			filtered = append(filtered, t)
		}
		allowedTools = filtered
		if len(dropped) > 0 {
			logger.Warnf(ctx, "Dropped wiki tools %v because no wiki-capable KB is in scope", dropped)
		}
	}
	if !hasVectorKB {
		filtered := make([]string, 0, len(allowedTools))
		dropped := make([]string, 0)
		for _, t := range allowedTools {
			if ragToolSet[t] || (documentToolSet[t] && !hasWikiKB) {
				dropped = append(dropped, t)
				continue
			}
			filtered = append(filtered, t)
		}
		allowedTools = filtered
		if len(dropped) > 0 {
			logger.Warnf(ctx, "Dropped RAG tools %v because no RAG-capable KB is in scope", dropped)
		}
	}
	// The graph tool only answers on graph-enabled bases; offering it on a
	// scope without one produced degraded plain-search results and a tool
	// the model kept trying. It follows the graph capability instead.
	if !hasGraphKB {
		if trimmed := withoutString(allowedTools, tools.ToolQueryKnowledgeGraph); len(trimmed) != len(allowedTools) {
			allowedTools = trimmed
			logger.Infof(ctx, "Dropped query_knowledge_graph because no graph-enabled KB is in scope")
		}
	}

	// Deduplicate while preserving original order.
	allowedTools = dedupStrings(allowedTools)

	// Register each allowed tool
	for _, toolName := range allowedTools {
		var toolToRegister types.Tool

		switch toolName {
		case tools.ToolThinking:
			toolToRegister = tools.NewSequentialThinkingTool()
		case tools.ToolTodoWrite:
			toolToRegister = tools.NewTodoWriteTool()
		case tools.ToolSearchKnowledge:
			toolToRegister = tools.NewSearchKnowledgeTool(
				s.knowledgeBaseService,
				s.knowledgeService,
				s.chunkService,
				config.SearchTargets,
				rerankModel,
				s.cfg,
			).WithContextImages(config.ChatModelSupportsVision)
		case tools.ToolReadDocument:
			toolToRegister = tools.NewReadDocumentTool(s.knowledgeService, s.chunkService, config.SearchTargets)
		case tools.ToolListDocuments:
			toolToRegister = tools.NewListDocumentsTool(s.knowledgeService, config.SearchTargets)
		case tools.ToolQueryKnowledgeGraph:
			var chunkRepo interfaces.ChunkRepository
			if s.chunkService != nil {
				chunkRepo = s.chunkService.GetRepository()
			}
			toolToRegister = tools.NewQueryKnowledgeGraphTool(s.knowledgeBaseService, config.SearchTargets).
				WithKnowledgeScope(s.knowledgeService).
				WithGraph(s.graphRepo, chunkRepo)
		case tools.ToolDatabaseQuery:
			toolToRegister = tools.NewDatabaseQueryTool(s.db, config.SearchTargets)
		case tools.ToolDataAnalysis:
			toolToRegister = tools.NewDataAnalysisTool(s.knowledgeBaseService, s.knowledgeService, s.tenantService, s.fileService, s.duckdb, sessionID, s.storageResolver).
				WithSearchTargets(config.SearchTargets)
			logger.Infof(ctx, "Registered data_analysis tool for session: %s", sessionID)

		case tools.ToolDataSchema:
			toolToRegister = tools.NewDataSchemaTool(s.knowledgeService, s.chunkService.GetRepository()).
				WithSearchTargets(config.SearchTargets)
			logger.Infof(ctx, "Registered data_schema tool")

		// Wiki tools — only registered when wiki KBs are detected
		case tools.ToolWikiReadPage:
			toolToRegister = tools.NewWikiReadPageTool(s.wikiPageService, s.knowledgeService, wikiScopes, wikiRoutes)
		case tools.ToolWikiSearch:
			toolToRegister = tools.NewWikiSearchTool(s.wikiPageService, s.knowledgeService, wikiScopes, wikiRoutes)
		case tools.ToolWikiFlagIssue:
			toolToRegister = tools.NewWikiFlagIssueTool(s.wikiPageService, writableWikiKBIDs, wikiRoutes).
				WithKnowledgeScope(s.knowledgeService, config.SearchTargets)
		case tools.ToolWikiReadIssue:
			toolToRegister = tools.NewWikiReadIssueTool(s.wikiPageService, wikiKBIDs)
		case tools.ToolWikiUpdateIssue:
			toolToRegister = tools.NewWikiUpdateIssueTool(s.wikiPageService, writableWikiKBIDs)
		case tools.ToolWikiWritePage:
			toolToRegister = tools.NewWikiWritePageTool(
				s.wikiPageService, writableWikiKBIDs, s.knowledgeService, wikiRoutes,
			).WithSearchTargets(config.SearchTargets)
		case tools.ToolWikiReplaceText:
			toolToRegister = tools.NewWikiReplaceTextTool(
				s.wikiPageService, writableWikiKBIDs, s.knowledgeService, wikiRoutes,
			).WithSearchTargets(config.SearchTargets)
		case tools.ToolWikiRenamePage:
			toolToRegister = tools.NewWikiRenamePageTool(s.wikiPageService, writableWikiKBIDs, wikiRoutes)
		case tools.ToolWikiDeletePage:
			toolToRegister = tools.NewWikiDeletePageTool(s.wikiPageService, writableWikiKBIDs, wikiRoutes)

		default:
			logger.Warnf(ctx, "Unknown tool: %s", toolName)
		}

		if toolToRegister != nil {
			if toolToRegister.Name() != toolName {
				logger.Warnf(ctx, "Tool name mismatch: expected %s, got %s", toolName, toolToRegister.Name())
			}
			registry.RegisterTool(toolToRegister)
		}
	}

	logger.Infof(ctx, "Registered %d tools", len(registry.ListTools()))
	return nil
}

// withoutWikiWriteTools drops the tools that write Wiki state. They execute
// inside the agent engine and so bypass the HTTP KB permission middleware; they
// are removed for shared agents (read-only by contract) and whenever no
// writable wiki KB is in scope.
func withoutWikiWriteTools(allowed []string) []string {
	wikiWrites := map[string]bool{
		tools.ToolWikiFlagIssue:   true,
		tools.ToolWikiUpdateIssue: true,
		tools.ToolWikiWritePage:   true,
		tools.ToolWikiReplaceText: true,
		tools.ToolWikiRenamePage:  true,
		tools.ToolWikiDeletePage:  true,
	}
	filtered := make([]string, 0, len(allowed))
	for _, name := range allowed {
		if !wikiWrites[name] {
			filtered = append(filtered, name)
		}
	}
	return filtered
}

// ValidateConfig validates the agent configuration
func (s *agentService) ValidateConfig(config *types.AgentConfig) error {
	if config == nil {
		return fmt.Errorf("config cannot be nil")
	}

	if config.MaxIterations < 0 {
		config.MaxIterations = types.UnlimitedMaxIterations
	} else if config.MaxIterations == 0 {
		config.MaxIterations = 5
	} else if config.MaxIterations > MAX_ITERATIONS {
		return fmt.Errorf("max iterations too high: %d (max %d)", config.MaxIterations, MAX_ITERATIONS)
	}

	return nil
}

// getKnowledgeBaseInfos retrieves detailed information for knowledge bases.
// kbTenantMap carries the tenant each KB should be queried under (source tenant
// for directly shared KBs); a missing entry falls back to the request tenant.
func (s *agentService) getKnowledgeBaseInfos(ctx context.Context, kbIDs []string, kbTenantMap map[string]uint64) ([]*agent.KnowledgeBaseInfo, error) {
	if len(kbIDs) == 0 {
		return []*agent.KnowledgeBaseInfo{}, nil
	}

	kbInfos := make([]*agent.KnowledgeBaseInfo, 0, len(kbIDs))

	for _, kbID := range kbIDs {
		// Get knowledge base details
		kb, err := s.knowledgeBaseService.GetKnowledgeBaseByID(ctx, kbID)
		if err != nil {
			logger.Warnf(ctx, "Failed to get knowledge base %s: %v", secutils.SanitizeForLog(kbID), err)
			kbInfos = append(kbInfos, &agent.KnowledgeBaseInfo{
				ID:          kbID,
				Name:        kbID,
				Type:        "document",
				Description: "",
				DocCount:    0,
				RecentDocs:  []agent.RecentDocInfo{},
			})
			continue
		}

		// Skip hidden/system-managed knowledge bases (e.g., __chat_history__)
		if kb.IsTemporary {
			logger.Debugf(ctx, "Skipping temporary knowledge base %s (%s) from prompt", kb.ID, kb.Name)
			continue
		}

		// Document/FAQ listing below is tenant-scoped, so a directly shared KB
		// must be queried under its source tenant — the request context belongs
		// to the receiving tenant and would silently yield doc_count=0. The
		// tenant comes from the SearchTarget that buildSearchTargets already
		// authorized; this only widens the metadata query, never the KB set.
		metaCtx := ctx
		if scopeTenantID := kbTenantMap[kbID]; scopeTenantID != 0 {
			metaCtx = context.WithValue(ctx, types.TenantIDContextKey, scopeTenantID)
		}

		// Get document count and recent documents
		docCount := 0
		recentDocs := []agent.RecentDocInfo{}

		if kb.Type == types.KnowledgeBaseTypeFAQ {
			pageResult, err := s.knowledgeService.ListFAQEntries(metaCtx, kbID, &types.Pagination{
				Page:     1,
				PageSize: 10,
			}, nil, 0, "", "", "", nil)
			if err == nil && pageResult != nil {
				docCount = int(pageResult.Total)
				if entries, ok := pageResult.Data.([]*types.FAQEntry); ok {
					for _, entry := range entries {
						if len(recentDocs) >= 10 {
							break
						}
						recentDocs = append(recentDocs, agent.RecentDocInfo{
							ChunkID:             entry.ChunkID,
							KnowledgeID:         entry.KnowledgeID,
							KnowledgeBaseID:     entry.KnowledgeBaseID,
							Title:               entry.StandardQuestion,
							Type:                string(types.ChunkTypeFAQ),
							CreatedAt:           entry.CreatedAt.Format("2006-01-02"),
							FAQStandardQuestion: entry.StandardQuestion,
							FAQSimilarQuestions: entry.SimilarQuestions,
							FAQAnswers:          entry.Answers,
						})
					}
				}
			} else if err != nil {
				logger.Warnf(ctx, "Failed to list FAQ entries for %s: %v", kbID, err)
			}
		}

		// Fallback to generic knowledge listing when not FAQ or FAQ retrieval failed
		if kb.Type != types.KnowledgeBaseTypeFAQ || len(recentDocs) == 0 {
			pageResult, err := s.knowledgeService.ListPagedKnowledgeByKnowledgeBaseID(metaCtx, kbID, &types.Pagination{
				Page:     1,
				PageSize: 10,
			}, types.KnowledgeListFilter{
				ParseStatus: types.ParseStatusCompleted,
			})

			if err == nil && pageResult != nil {
				docCount = int(pageResult.Total)

				// Convert to Knowledge slice
				if knowledges, ok := pageResult.Data.([]*types.Knowledge); ok {
					for _, k := range knowledges {
						if len(recentDocs) >= 10 {
							break
						}
						recentDocs = append(recentDocs, agent.RecentDocInfo{
							KnowledgeID: k.ID,
							Title:       k.Title,
							Description: k.Description,
							FileName:    k.FileName,
							Type:        k.FileType,
							CreatedAt:   k.CreatedAt.Format("2006-01-02"),
							FileSize:    k.FileSize,
						})
					}
				}
			}
		}

		kbType := kb.Type
		if kbType == "" {
			kbType = "document" // Default type
		}
		var profile *types.KnowledgeBaseProfile
		if kb.GeneratedProfile.HasText() {
			profile = kb.GeneratedProfile
		}
		kbInfos = append(kbInfos, &agent.KnowledgeBaseInfo{
			ID:           kb.ID,
			Name:         kb.Name,
			Type:         kbType,
			Description:  kb.Description,
			DocCount:     docCount,
			Capabilities: kbRetrievalCapabilities(kb),
			RecentDocs:   recentDocs,
			Profile:      profile,
		})
	}

	return kbInfos, nil
}

// kbRetrievalCapabilities reports which retrieval surfaces a KB exposes.
// Surfaces are the static facts the hybrid agent prompt consults to pick its
// retrieval strategy — the agent should NOT need to probe this via search.
//
// Returned values are a subset of {"wiki", "chunks"}:
//   - "wiki"   → the KB has wiki ingestion enabled (wiki_search / wiki_read_page)
//   - "chunks" → the KB has vector and/or keyword (BM25) indexing enabled
//     (knowledge_search / grep_chunks)
func kbRetrievalCapabilities(kb *types.KnowledgeBase) []string {
	if kb == nil {
		return nil
	}
	caps := make([]string, 0, 2)
	if kb.IsWikiEnabled() {
		caps = append(caps, "wiki")
	}
	if kb.IsVectorEnabled() || kb.IsKeywordEnabled() {
		caps = append(caps, "chunks")
	}
	return caps
}

// getSelectedDocumentInfos retrieves detailed information for user-selected documents (via @ mention)
// This loads the actual content of the documents to include in the system prompt
// resolveQuestionOriginInfo turns a suggested question's origin into the
// names the runtime context shows. It re-checks the base against this turn's
// search targets rather than trusting the caller, and keeps a document only
// when it belongs to that base. A base reached only through a document or tag
// scope is not in kbInfos when other bases are selected, so its name is
// looked up directly.
func (s *agentService) resolveQuestionOriginInfo(
	ctx context.Context, origin *types.QuestionOrigin, targets types.SearchTargets,
	kbInfos []*agent.KnowledgeBaseInfo,
) *agent.QuestionOriginInfo {
	if origin == nil || origin.KnowledgeBaseID == "" || !targets.ContainsKB(origin.KnowledgeBaseID) {
		return nil
	}
	info := &agent.QuestionOriginInfo{KnowledgeBaseID: origin.KnowledgeBaseID}
	for _, kb := range kbInfos {
		if kb != nil && kb.ID == origin.KnowledgeBaseID {
			info.KnowledgeBaseName = kb.Name
			break
		}
	}
	if info.KnowledgeBaseName == "" && s.knowledgeBaseService != nil {
		kb, err := s.knowledgeBaseService.GetKnowledgeBaseByID(ctx, origin.KnowledgeBaseID)
		if err == nil && kb != nil {
			info.KnowledgeBaseName = kb.Name
		}
	}
	if origin.KnowledgeID == "" {
		return info
	}
	docs, err := s.getSelectedDocumentInfos(ctx, []string{origin.KnowledgeID})
	if err != nil || len(docs) != 1 || docs[0].KnowledgeBaseID != origin.KnowledgeBaseID {
		logger.Infof(ctx, "Question origin document %s dropped: not found in knowledge base %s",
			secutils.SanitizeForLog(origin.KnowledgeID), secutils.SanitizeForLog(origin.KnowledgeBaseID))
		return info
	}
	info.Document = docs[0]
	return info
}

func (s *agentService) getSelectedDocumentInfos(ctx context.Context, knowledgeIDs []string) ([]*agent.SelectedDocumentInfo, error) {
	if len(knowledgeIDs) == 0 {
		return []*agent.SelectedDocumentInfo{}, nil
	}

	// Get tenant ID from context
	tenantID := uint64(0)
	if tid, ok := types.TenantIDFromContext(ctx); ok {
		tenantID = tid
	}

	// Fetch knowledge metadata (include docs from shared KBs the user has access to)
	knowledges, err := s.knowledgeService.GetKnowledgeBatchWithSharedAccess(ctx, tenantID, knowledgeIDs)
	if err != nil {
		return nil, fmt.Errorf("failed to get knowledge batch: %w", err)
	}

	// Build map for quick lookup
	knowledgeMap := make(map[string]*types.Knowledge)
	for _, k := range knowledges {
		if k != nil {
			knowledgeMap[k.ID] = k
		}
	}

	selectedDocs := make([]*agent.SelectedDocumentInfo, 0, len(knowledgeIDs))

	for _, kid := range knowledgeIDs {
		k, ok := knowledgeMap[kid]
		if !ok {
			logger.Warnf(ctx, "Selected knowledge %s not found", kid)
			continue
		}

		docInfo := &agent.SelectedDocumentInfo{
			KnowledgeID:     k.ID,
			KnowledgeBaseID: k.KnowledgeBaseID,
			Title:           k.Title,
			FileName:        k.FileName,
			FileType:        k.FileType,
		}

		selectedDocs = append(selectedDocs, docInfo)
	}

	logger.Infof(ctx, "Loaded %d selected documents metadata for prompt", len(selectedDocs))
	return selectedDocs, nil
}
