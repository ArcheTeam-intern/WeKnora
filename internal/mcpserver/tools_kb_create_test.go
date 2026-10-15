package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/mark3labs/mcp-go/mcp"
)

// recordingKBService captures what the create tool handed to the service.
func recordingKBService() *stubKBService {
	return &stubKBService{kbs: map[string]*types.KnowledgeBase{}}
}

// ingestEndpoint is an endpoint with the ingest tools enabled, which is what
// gives it the ingest capability the write path checks (and what the console's
// endpoint looks like in production).
func ingestEndpoint(id string, tenantID uint64) *types.MCPEndpoint {
	return &types.MCPEndpoint{ID: id, TenantID: tenantID, Tools: []string{
		types.MCPEndpointToolCreateKnowledgeBase,
		types.MCPEndpointToolUploadFile,
		types.MCPEndpointToolImportDocumentFile,
	}}
}

func createRequest(args map[string]any) mcp.CallToolRequest {
	return mcp.CallToolRequest{Params: mcp.CallToolParams{Arguments: args}}
}

// webUserCallContext is mcpCallContext plus a real web-user principal, which is
// what turns the role gate on (machine mode is capability-gated instead).
func webUserCallContext(tenantID uint64, ep *types.MCPEndpoint, role types.TenantRole, userID string) context.Context {
	ctx := mcpCallContext(tenantID, ep)
	ctx = types.WithPrincipal(ctx, types.Principal{Type: types.PrincipalWebUser, ID: userID})
	return types.WithCaller(ctx, types.Caller{TenantID: tenantID, UserID: userID, Role: role})
}

func TestCreateKnowledgeBaseRequiresName(t *testing.T) {
	srv := &Server{kbService: recordingKBService()}
	ep := ingestEndpoint("ep-1", 1)
	ctx := webUserCallContext(1, ep, types.TenantRoleAdmin, "u-1")

	result, err := srv.handleCreateKnowledgeBase(ctx, createRequest(map[string]any{"name": "   "}))
	if err != nil {
		t.Fatalf("handler returned transport error: %v", err)
	}
	if !result.IsError || !strings.Contains(textOf(result), "name is required") {
		t.Fatalf("expected a name-required error, got %#v", result.Content)
	}
}

// A Viewer may not create knowledge bases: the REST entry point uses the
// Contributor floor, and MCP must not be a way around it.
func TestCreateKnowledgeBaseRejectsViewer(t *testing.T) {
	stub := recordingKBService()
	srv := &Server{kbService: stub}
	ep := ingestEndpoint("ep-1", 1)
	ctx := webUserCallContext(1, ep, types.TenantRoleViewer, "u-viewer")

	result, err := srv.handleCreateKnowledgeBase(ctx, createRequest(map[string]any{"name": "不该存在的库"}))
	if err != nil {
		t.Fatalf("handler returned transport error: %v", err)
	}
	if !result.IsError {
		t.Fatalf("viewer creation must fail, got %#v", result.Content)
	}
	if !strings.Contains(textOf(result), "contributor") {
		t.Fatalf("error should name the required role, got %q", textOf(result))
	}
	if len(stub.created) != 0 {
		t.Fatalf("no knowledge base may be created for a viewer, got %d", len(stub.created))
	}
}

func TestCreateKnowledgeBaseMapsCapabilityFlags(t *testing.T) {
	stub := recordingKBService()
	srv := &Server{kbService: stub}
	ep := ingestEndpoint("ep-1", 1)
	ctx := webUserCallContext(1, ep, types.TenantRoleContributor, "u-contrib")

	result, err := srv.handleCreateKnowledgeBase(ctx, createRequest(map[string]any{
		"name":           "验证库-新",
		"description":    "created by the console",
		"enable_wiki":    true,
		"enable_graph":   true,
		"enable_keyword": false,
	}))
	if err != nil {
		t.Fatalf("handler returned transport error: %v", err)
	}
	if result.IsError {
		t.Fatalf("contributor creation failed: %q", textOf(result))
	}
	if len(stub.created) != 1 {
		t.Fatalf("expected exactly one created knowledge base, got %d", len(stub.created))
	}
	created := stub.created[0]
	if created.Name != "验证库-新" || created.Description != "created by the console" {
		t.Fatalf("name/description not forwarded: %+v", created)
	}
	strategy := created.IndexingStrategy
	if !strategy.VectorEnabled || strategy.KeywordEnabled || !strategy.WikiEnabled || !strategy.GraphEnabled {
		t.Fatalf("capability flags not mapped as asked: %+v", strategy)
	}
	// Graph extraction needs the extract config too, otherwise the pipeline
	// flag alone extracts nothing.
	if created.ExtractConfig == nil || !created.ExtractConfig.Enabled {
		t.Fatalf("graph request must enable ExtractConfig: %+v", created.ExtractConfig)
	}
	// The tool must hand the request context (caller identity) to the service,
	// which stamps tenant and creator from it; the stub records what it saw.
	if len(stub.createdFor) != 1 || stub.createdFor[0] != "u-contrib" {
		t.Fatalf("caller identity must reach the service: %+v", stub.createdFor)
	}
	if !strings.Contains(textOf(result), "验证库-新") {
		t.Fatalf("result should describe the new knowledge base, got %q", textOf(result))
	}
}

// A restricted endpoint cannot use what it just created, so the response says
// so instead of letting the next call fail with "out of scope".
// rangeRecordingRepo captures the Update call that extends the endpoint range.
type rangeRecordingRepo struct {
	interfaces.MCPEndpointRepository
	updated *types.MCPEndpoint
}

func (r *rangeRecordingRepo) Update(_ context.Context, ep *types.MCPEndpoint) error {
	updated := *ep
	r.updated = &updated
	return nil
}

func TestCreateKnowledgeBaseReportsEndpointScope(t *testing.T) {
	stub := recordingKBService()
	repo := &rangeRecordingRepo{}
	srv := &Server{kbService: stub, endpointRepo: repo}
	ep := ingestEndpoint("ep-restricted", 1)
	ep.KnowledgeBaseIDs = []string{"kb-existing"}
	ctx := webUserCallContext(1, ep, types.TenantRoleAdmin, "u-admin")

	result, err := srv.handleCreateKnowledgeBase(ctx, createRequest(map[string]any{"name": "范围外的新库"}))
	if err != nil {
		t.Fatalf("handler returned transport error: %v", err)
	}
	if result.IsError {
		t.Fatalf("creation failed: %q", textOf(result))
	}
	if !strings.Contains(textOf(result), "endpoint_scope") {
		t.Fatalf("restricted endpoint must be reported: %q", textOf(result))
	}
	if repo.updated == nil {
		t.Fatal("the endpoint range update must be persisted")
	}
	if len(repo.updated.KnowledgeBaseIDs) != 2 || !containsID(repo.updated.KnowledgeBaseIDs, stub.created[0].ID) {
		t.Fatalf("new knowledge base must be appended to the range: %+v", repo.updated.KnowledgeBaseIDs)
	}
	if len(ep.KnowledgeBaseIDs) != 1 {
		t.Fatalf("the request's endpoint must not be mutated in place: %+v", ep.KnowledgeBaseIDs)
	}

	if !strings.Contains(textOf(result), "added_to_endpoint_range") {
		t.Fatalf("restricted endpoint should report the range update: %q", textOf(result))
	}

	// Same call on an unrestricted endpoint stays quiet about ranges.
	open := ingestEndpoint("ep-open", 1)
	result, err = srv.handleCreateKnowledgeBase(
		webUserCallContext(1, open, types.TenantRoleAdmin, "u-admin"),
		createRequest(map[string]any{"name": "无范围限制的库"}))
	if err != nil {
		t.Fatalf("handler returned transport error: %v", err)
	}
	if strings.Contains(textOf(result), "endpoint_scope") {
		t.Fatalf("unrestricted endpoint must not warn about ranges: %q", textOf(result))
	}
}

// A scoped API key without the ingest capability is refused, mirroring
// access.RequireKBWrite's machine-mode rule.
func TestCreateKnowledgeBaseRejectsKeyWithoutIngest(t *testing.T) {
	stub := recordingKBService()
	srv := &Server{kbService: stub}
	ep := ingestEndpoint("ep-1", 1)
	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(1))
	ctx = context.WithValue(ctx, types.MCPEndpointContextKey, ep)
	ctx = types.WithTenantAPIKeyScope(ctx, types.TenantAPIKeyScope{
		ScopeType:    types.APIKeyScopeTenant,
		Capabilities: types.StringArray{string(types.APIKeyCapabilityRetrieve)},
	})
	ctx = types.WithCaller(ctx, types.Caller{TenantID: 1, UserID: "key-user", Role: types.TenantRoleViewer})

	result, err := srv.handleCreateKnowledgeBase(ctx, createRequest(map[string]any{"name": "只读密钥建的库"}))
	if err != nil {
		t.Fatalf("handler returned transport error: %v", err)
	}
	if !result.IsError {
		t.Fatalf("read-only key must not create knowledge bases, got %#v", result.Content)
	}
	if len(stub.created) != 0 {
		t.Fatalf("nothing may be created for a read-only key, got %d", len(stub.created))
	}
}

// 图谱示例：实体类型直接给，关系用 "起点,终点,关系" 写法；写错的条目跳过而不是整体失败。
func TestGraphExtractionConfigParsesEntitiesAndRelations(t *testing.T) {
	req := createRequest(map[string]any{
		"graph_entities":  []any{"人员", "项目", " "},
		"graph_relations": []any{"人员,项目,负责", "写错了", "项目,服务,依赖"},
	})
	cfg := graphExtractionConfig(req)
	if cfg == nil || !cfg.Enabled {
		t.Fatalf("graph config must be enabled: %+v", cfg)
	}
	if len(cfg.Nodes) != 2 || cfg.Nodes[0].Name != "人员" || cfg.Nodes[1].Name != "项目" {
		t.Fatalf("entities not parsed as asked: %+v", cfg.Nodes)
	}
	if len(cfg.Relations) != 2 {
		t.Fatalf("unparsable relation entries must be skipped, got %+v", cfg.Relations)
	}
	first := cfg.Relations[0]
	if first.Node1 != "人员" || first.Node2 != "项目" || first.Type != "负责" {
		t.Fatalf("relation not parsed as asked: %+v", first)
	}
}

// textOf flattens a tool result's text content for assertions.
func textOf(result *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range result.Content {
		if text, ok := c.(mcp.TextContent); ok {
			b.WriteString(text.Text)
		}
	}
	if raw, err := json.Marshal(result.StructuredContent); err == nil && result.StructuredContent != nil {
		b.Write(raw)
	}
	return b.String()
}
