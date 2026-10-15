// Package mcpserver hosts WeKnora's own MCP server surface: the Streamable
// HTTP endpoint that external MCP clients (Claude Desktop, Cursor, VS Code
// Copilot, ...) connect to. Every request is authenticated by
// middleware.MCPEndpointAuth against a workspace MCPEndpoint row, and the
// endpoint's tool allowlist and knowledge-base scope decide what the client
// can see and call.
//
// A single mcp-go server instance is registered with the full tool catalog;
// per-endpoint visibility is applied through a tool filter (tools/list) and a
// handler middleware (tools/call), both keyed off the endpoint on the request
// context. The transport runs stateless so any replica can answer any call.
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/ratelimit"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/Tencent/WeKnora/internal/utils"
	"github.com/google/uuid"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

const (
	serverName    = "weknora"
	serverVersion = "1.0.0"

	rateLimitKeyPrefix = "mcp:endpoint:ratelimit:"
	lastUsedTouchEvery = time.Minute
)

var errNoEndpoint = errors.New("mcp endpoint missing from request context")

// Server wires the tool catalog onto an mcp-go server and exposes it as an
// http.Handler.
type Server struct {
	kbService        interfaces.KnowledgeBaseService
	knowledgeService interfaces.KnowledgeService
	chunkService     interfaces.ChunkService
	wikiService      interfaces.WikiPageService
	sessionService   interfaces.SessionService
	messageService   interfaces.MessageService
	agentService     interfaces.CustomAgentService
	kbShareService   interfaces.KBShareService
	tenantService    interfaces.TenantService
	resourceCatalog  interfaces.ResourceCatalog
	fileService      interfaces.FileService
	storageResolver  interfaces.StorageBackendResolver
	// graphRepo backs query_knowledge_graph. It is nil when the deployment has
	// no graph store wired at all; the graph tools then report the capability
	// as unavailable instead of returning an empty result.
	graphRepo interfaces.RetrieveGraphRepository
	// documentReader backs list_parser_engines. It is nil when docreader is
	// not wired; the engine tool then reports only the always-available
	// built-in engines instead of the remote catalog.
	documentReader interfaces.DocumentReader
	endpointRepo   interfaces.MCPEndpointRepository
	db             *gorm.DB
	cfg            *config.Config
	// stagingDir is the upload staging root. WeKnora owns this directory: the
	// upload transport writes the bytes itself and removes them once the import
	// receipt is persisted. There is no external writer and no shared volume.
	stagingDir string
	// uploadStagingOnce/uploadStagingSvc build the staging service lazily from
	// stagingDir + db, so a Server constructed without a database (unit tests,
	// degraded startup) has no upload surface instead of panicking.
	uploadStagingOnce sync.Once
	uploadStagingSvc  UploadStaging

	limiter   *ratelimit.Limiter
	lastTouch sync.Map // endpoint id -> time.Time of the last last_used_at write

	mcp     *server.MCPServer
	handler http.Handler
}

// NewServer builds the MCP server and its Streamable HTTP transport.
func NewServer(
	kbService interfaces.KnowledgeBaseService,
	knowledgeService interfaces.KnowledgeService,
	chunkService interfaces.ChunkService,
	wikiService interfaces.WikiPageService,
	sessionService interfaces.SessionService,
	messageService interfaces.MessageService,
	agentService interfaces.CustomAgentService,
	kbShareService interfaces.KBShareService,
	tenantService interfaces.TenantService,
	endpointRepo interfaces.MCPEndpointRepository,
	db *gorm.DB,
	cfg *config.Config,
	redisClient *redis.Client,
	resourceCatalog interfaces.ResourceCatalog,
	fileService interfaces.FileService,
	storageResolver interfaces.StorageBackendResolver,
	graphRepo interfaces.RetrieveGraphRepository,
	documentReader interfaces.DocumentReader,
) *Server {
	s := &Server{
		kbService:        kbService,
		knowledgeService: knowledgeService,
		chunkService:     chunkService,
		wikiService:      wikiService,
		sessionService:   sessionService,
		messageService:   messageService,
		agentService:     agentService,
		kbShareService:   kbShareService,
		tenantService:    tenantService,
		endpointRepo:     endpointRepo,
		db:               db,
		cfg:              cfg,
		resourceCatalog:  resourceCatalog,
		fileService:      fileService,
		storageResolver:  storageResolver,
		graphRepo:        graphRepo,
		documentReader:   documentReader,
		stagingDir:       strings.TrimSpace(os.Getenv(StagingEnvVar)),
		limiter:          ratelimit.New(redisClient, rateLimitKeyPrefix, time.Minute, ""),
	}
	// The local fallback map only grows without periodic eviction; the server
	// lives for the whole process so the cleanup goroutine never stops.
	go s.limiter.StartCleanup(make(chan struct{}))
	s.mcp = server.NewMCPServer(
		serverName,
		serverVersion,
		server.WithToolCapabilities(false),
		server.WithRecovery(),
		server.WithToolFilter(s.filterTools),
		server.WithToolHandlerMiddleware(s.guardTool),
		server.WithInstructions(serverInstructions),
	)
	s.mcp.AddTools(s.toolCatalog()...)
	s.handler = server.NewStreamableHTTPServer(
		s.mcp,
		server.WithStateLess(true),
	)
	return s
}

// Handler returns the http.Handler to mount under /mcp/:endpoint_id. The
// caller must run MCPEndpointAuth first; the handler trusts the context.
func (s *Server) Handler() http.Handler {
	return s.handler
}

// uploadStaging returns the staging service, or nil when this Server has no
// database or staging root configured.
func (s *Server) uploadStaging() UploadStaging {
	s.uploadStagingOnce.Do(func() {
		if s.db == nil {
			return
		}
		root := strings.TrimSpace(s.stagingDir)
		if root == "" {
			logger.Warnf(context.Background(),
				"[mcpserver] %s is not configured; the upload endpoint is disabled", StagingEnvVar)
			return
		}
		s.uploadStagingSvc = NewStagedUploadStorage(root, NewUploadReceiptRepository(s.db))
	})
	return s.uploadStagingSvc
}

// NewStagingSweepRunner builds the production cleanup scheduler for this
// server's staging area. The runner resolves the staging service per tick, so
// it stays dormant on servers without a database or staging root instead of
// needing separate wiring.
func (s *Server) NewStagingSweepRunner() *StagingSweepRunner {
	return NewStagingSweepRunner(s.uploadStaging)
}

// fileSizeLimit is the upload ceiling in bytes, taken from the same
// MAX_FILE_SIZE_MB setting the existing HTTP upload handler uses so the two
// entries cannot disagree.
func (s *Server) fileSizeLimit() int64 {
	return utils.GetMaxFileSize()
}

// newUploadID mints an upload id. UUID v4 keeps it collision-free and matches the
// shape the upload-id validator accepts.
func newUploadID() string {
	return uuid.New().String()
}

const serverInstructions = "WeKnora knowledge workspace. Start with list_knowledge_bases to see what is in scope, " +
	"then use search_knowledge for semantic questions, grep_chunks for exact keywords, read_document to read " +
	"a whole document, and ask to get a synthesized answer with citations. Wiki tools browse the generated " +
	"wiki when a knowledge base has one (wiki_index for the directory, wiki_list_pages to page through it, " +
	"wiki_graph for how pages link to each other). query_knowledge_graph explores the extracted entity " +
	"relationships and returns the evidence chunks behind them. get_document_status and " +
	"get_knowledge_base_status report how far indexing has actually progressed. Write tools " +
	"(add/update/delete_document, import_document_file) exist only on endpoints that enabled them."

// endpointFromContext returns the endpoint authenticated for this call.
func endpointFromContext(ctx context.Context) (*types.MCPEndpoint, error) {
	ep, ok := ctx.Value(types.MCPEndpointContextKey).(*types.MCPEndpoint)
	if !ok || ep == nil {
		return nil, errNoEndpoint
	}
	return ep, nil
}

// filterTools hides catalog entries the endpoint did not enable.
func (s *Server) filterTools(ctx context.Context, all []mcp.Tool) []mcp.Tool {
	ep, err := endpointFromContext(ctx)
	if err != nil {
		return nil
	}
	out := make([]mcp.Tool, 0, len(all))
	for _, t := range all {
		if ep.HasTool(t.Name) {
			out = append(out, t)
		}
	}
	return out
}

// scopeOptionalTools may be called before a knowledge-base selection exists.
//
// list_knowledge_bases is the discovery tool: it is how a client learns which
// knowledge bases it could select, so requiring a selection for it would make
// the selection impossible to obtain. Every other knowledge tool reads or writes
// content and therefore requires an explicit selection whenever the caller sent
// the selection header.
var scopeOptionalTools = map[string]struct{}{
	types.MCPEndpointToolListParserEngines:  {},
	types.MCPEndpointToolListKnowledgeBases: {},
	// Creating a knowledge base is how a caller gets something to select.
	types.MCPEndpointToolCreateKnowledgeBase: {},
	types.MCPEndpointToolGetDocumentStatus:   {},
	types.MCPEndpointToolGetKBStatus:         {},
	types.MCPEndpointToolGetUploadStatus:     {},
	types.MCPEndpointToolImportDocumentFile:  {},
}

// guardTool enforces the allowlist, the request scope and the rate limit on
// every tools/call, so a client cannot invoke a hidden tool by name.
func (s *Server) guardTool(next server.ToolHandlerFunc) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ep, err := endpointFromContext(ctx)
		if err != nil {
			return mcp.NewToolResultError("unauthorized"), nil
		}
		if !ep.HasTool(req.Params.Name) {
			return errorResult(&ToolError{
				Code:    CodeForbidden,
				Message: fmt.Sprintf("tool %q is not enabled on this endpoint", req.Params.Name),
			}), nil
		}
		// An explicitly empty selection must not be read as "everything": a tool
		// that needs a knowledge base has to get one.
		if _, optional := scopeOptionalTools[req.Params.Name]; !optional {
			if err := RequestScopeOf(ctx).RequireSelection(req.Params.Name); err != nil {
				return errorResult(err), nil
			}
		}
		if !s.limiter.Allow(ctx, ep.ID, ep.RateLimitPerMinute) {
			return errorResult(&ToolError{
				Code:      CodeRateLimited,
				Message:   "rate limit exceeded for this endpoint, retry shortly",
				Retryable: true,
			}), nil
		}
		s.touchLastUsed(ctx, ep.ID)
		result, err := next(ctx, req)
		if err != nil {
			return result, err
		}
		return s.rewriteResourceURLs(ctx, ep, result), nil
	}
}

// touchLastUsed records activity at most once a minute per endpoint.
func (s *Server) touchLastUsed(ctx context.Context, endpointID string) {
	if s.endpointRepo == nil {
		return
	}
	now := time.Now()
	if prev, ok := s.lastTouch.Load(endpointID); ok {
		if last, ok := prev.(time.Time); ok && now.Sub(last) < lastUsedTouchEvery {
			return
		}
	}
	s.lastTouch.Store(endpointID, now)
	go func() {
		bg, cancel := context.WithTimeout(logger.CloneContext(ctx), 5*time.Second)
		defer cancel()
		if err := s.endpointRepo.TouchLastUsed(bg, endpointID); err != nil {
			logger.Warnf(bg, "[mcpserver] touch last_used failed for %s: %v", endpointID, err)
		}
	}()
}

// toolCatalog lists every tool with its handler. Order matches the settings
// catalog so tools/list is stable.
func (s *Server) toolCatalog() []server.ServerTool {
	return []server.ServerTool{
		{Tool: listKnowledgeBasesTool(), Handler: s.handleListKnowledgeBases},
		{Tool: createKnowledgeBaseTool(), Handler: s.handleCreateKnowledgeBase},
		{Tool: listParserEnginesTool(), Handler: s.handleListParserEngines},
		{Tool: searchKnowledgeTool(), Handler: s.handleSearchKnowledge},
		{Tool: grepChunksTool(), Handler: s.handleGrepChunks},
		{Tool: listDocumentsTool(), Handler: s.handleListDocuments},
		{Tool: readDocumentTool(), Handler: s.handleReadDocument},
		{Tool: askTool(), Handler: s.handleAsk},
		{Tool: wikiSearchTool(), Handler: s.handleWikiSearch},
		{Tool: wikiReadPageTool(), Handler: s.handleWikiReadPage},
		{Tool: wikiIndexTool(), Handler: s.handleWikiIndex},
		{Tool: wikiListPagesTool(), Handler: s.handleWikiListPages},
		{Tool: wikiGraphTool(), Handler: s.handleWikiGraph},
		{Tool: knowledgeGraphQueryTool(), Handler: s.handleKnowledgeGraphQuery},
		{Tool: getDocumentStatusTool(), Handler: s.handleGetDocumentStatus},
		{Tool: getKnowledgeBaseStatusTool(), Handler: s.handleGetKnowledgeBaseStatus},
		{Tool: getUploadStatusTool(), Handler: s.handleGetUploadStatus},
		{Tool: uploadFileTool(), Handler: s.handleUploadFile},
		{Tool: importDocumentFileTool(), Handler: s.handleImportDocumentFile},
		{Tool: addDocumentTool(), Handler: s.handleAddDocument},
		{Tool: updateDocumentTool(), Handler: s.handleUpdateDocument},
		{Tool: deleteDocumentTool(), Handler: s.handleDeleteDocument},
	}
}

// toolResultFromAgentTool converts an agent tool result into an MCP result.
// Agent tools already format their Output for a model reader, so the text
// is passed through; Data is attached as structured content when present.
func toolResultFromAgentTool(res *types.ToolResult, err error) *mcp.CallToolResult {
	if err != nil && (res == nil || res.Error == "") {
		return mcp.NewToolResultError(err.Error())
	}
	if res == nil {
		return mcp.NewToolResultError("tool returned no result")
	}
	if !res.Success {
		msg := res.Error
		if msg == "" {
			msg = res.Output
		}
		if msg == "" {
			msg = "tool failed"
		}
		return mcp.NewToolResultError(msg)
	}
	if len(res.Data) > 0 {
		return mcp.NewToolResultStructured(res.Data, res.Output)
	}
	return mcp.NewToolResultText(res.Output)
}
