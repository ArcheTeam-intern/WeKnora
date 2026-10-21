package mcpserver

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

func TestKnowledgeOnlyAgentNeverFallsBackToNativeDefaultTools(t *testing.T) {
	source := &types.CustomAgent{Config: types.CustomAgentConfig{AllowedTools: []string{tools.ToolShellExec}, WebSearchEnabled: true, MCPSelectionMode: "all", SkillsSelectionMode: "all", SandboxConfigID: "sandbox"}}
	agent, err := knowledgeOnlyAgent(source)
	require.Error(t, err, "an explicitly configured agent with no usable knowledge tools must not inherit native defaults")
	require.Nil(t, agent)
	require.True(t, source.Config.WebSearchEnabled, "selection must not mutate stored configuration")
}

type workspaceAgents struct {
	interfaces.CustomAgentService
	rows   map[string]*types.CustomAgent
	writes int
}

func (s *workspaceAgents) ListAgents(context.Context) ([]*types.CustomAgent, error) {
	out := []*types.CustomAgent{}
	for _, row := range s.rows {
		out = append(out, row)
	}
	return out, nil
}
func (s *workspaceAgents) GetAgentByID(_ context.Context, id string) (*types.CustomAgent, error) {
	if row := s.rows[id]; row != nil {
		return row, nil
	}
	return nil, notFound("agent not found")
}
func (s *workspaceAgents) CreateAgent(ctx context.Context, row *types.CustomAgent) (*types.CustomAgent, error) {
	s.writes++
	row.ID = "new-agent"
	row.TenantID = types.MustTenantIDFromContext(ctx)
	row.CreatedBy, _ = types.UserIDFromContext(ctx)
	s.rows[row.ID] = row
	return row, nil
}
func (s *workspaceAgents) UpdateAgent(_ context.Context, row *types.CustomAgent, _ *string) (*types.CustomAgent, error) {
	s.writes++
	s.rows[row.ID] = row
	return row, nil
}
func (s *workspaceAgents) DeleteAgent(_ context.Context, id string) error {
	s.writes++
	delete(s.rows, id)
	return nil
}
func (s *workspaceAgents) CopyAgent(ctx context.Context, id string) (*types.CustomAgent, error) {
	row := *s.rows[id]
	row.ID = "copied-agent"
	row.IsBuiltin = false
	row.Name += " (copy)"
	row.CreatedBy, _ = types.UserIDFromContext(ctx)
	s.writes++
	s.rows[row.ID] = &row
	return &row, nil
}

func workspaceCall(t *testing.T, srv *Server, ctx context.Context, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	for _, tool := range srv.coreToolCatalog() {
		if tool.Tool.Name == "agent_workspace" {
			result, err := tool.Handler(ctx, legacyRequest(args))
			require.NoError(t, err)
			return result
		}
	}
	t.Fatal("agent_workspace is missing from the native core catalog")
	return nil
}

func workspaceFixture() (*Server, *workspaceAgents, *types.MCPEndpoint, context.Context) {
	agents := &workspaceAgents{rows: map[string]*types.CustomAgent{
		"own":                      {ID: "own", TenantID: 1, Name: "Own", CreatedBy: "owner", Config: types.CustomAgentConfig{AgentMode: "quick-answer", ModelID: "native-model", ContextTemplate: "keep-template", KnowledgeBases: []string{"kb-1"}, KBSelectionMode: "selected"}},
		types.BuiltinQuickAnswerID: {ID: types.BuiltinQuickAnswerID, IsBuiltin: true, Config: types.CustomAgentConfig{AgentMode: "quick-answer", ModelID: "native-model"}},
		"foreign":                  {ID: "foreign", TenantID: 2, Name: "Hidden"},
	}}
	srv, _ := planAskServer()
	srv.agentService = agents
	ep := planAskEndpoint()
	ep.Tools = append(ep.Tools, "agent_workspace")
	ctx := webUserCallContext(1, ep, types.TenantRoleContributor, "owner")
	ctx = context.WithValue(ctx, types.UserIDContextKey, "owner")
	return srv, agents, ep, ctx
}

func TestAgentWorkspaceNativeCatalogAndDefaultOffPermission(t *testing.T) {
	srv, _, ep, ctx := workspaceFixture()
	result := workspaceCall(t, srv, ctx, map[string]any{"action": "list"})
	require.False(t, result.IsError, mcpErrorText(result))
	rows := structuredValue(structuredOf(result)["agents"]).([]any)
	require.Len(t, rows, 2)
	for _, row := range rows {
		dto := row.(map[string]any)
		require.NotContains(t, dto, "tenant_id")
		require.NotContains(t, dto["config"], "context_template")
	}
	ep.Tools = []string{types.MCPEndpointToolAsk}
	require.True(t, workspaceCall(t, srv, ctx, map[string]any{"action": "list"}).IsError)
	for _, name := range types.DefaultMCPEndpointTools() {
		require.NotEqual(t, "agent_workspace", name)
	}
}

func TestAgentWorkspaceRejectsMachineAndViewerWrites(t *testing.T) {
	srv, agents, ep, _ := workspaceFixture()
	for _, ctx := range []context.Context{mcpCallContext(1, ep), webUserCallContext(1, ep, types.TenantRoleViewer, "owner")} {
		result := workspaceCall(t, srv, ctx, map[string]any{"action": "create", "name": "New"})
		require.True(t, result.IsError)
	}
	require.Zero(t, agents.writes)
}

func TestAgentWorkspaceCreateUpdateAndCopyPreserveNativeOwnership(t *testing.T) {
	srv, agents, _, ctx := workspaceFixture()
	result := workspaceCall(t, srv, ctx, map[string]any{"action": "create", "name": "New", "config": map[string]any{"kb_selection_mode": "selected", "knowledge_bases": []any{"kb-1"}}})
	require.False(t, result.IsError, mcpErrorText(result))
	require.Equal(t, "owner", agents.rows["new-agent"].CreatedBy)
	require.Equal(t, "native-model", agents.rows["new-agent"].Config.ModelID)
	result = workspaceCall(t, srv, ctx, map[string]any{"action": "update", "agent_id": "own", "description": "Edited", "config": map[string]any{"system_prompt": "answer carefully"}})
	require.False(t, result.IsError, mcpErrorText(result))
	require.Equal(t, "keep-template", agents.rows["own"].Config.ContextTemplate)
	require.Equal(t, "owner", agents.rows["own"].CreatedBy)
	result = workspaceCall(t, srv, ctx, map[string]any{"action": "copy", "agent_id": "own", "name": "My copy"})
	require.False(t, result.IsError, mcpErrorText(result))
	require.Equal(t, "My copy", agents.rows["copied-agent"].Name)
	require.Equal(t, "owner", agents.rows["copied-agent"].CreatedBy)
}

func TestAgentWorkspaceRefusesScopeExpansionAndUnsupportedConfig(t *testing.T) {
	for _, config := range []map[string]any{
		{"allowed_tools": []any{"shell_exec"}}, {"sandbox_config_id": "sandbox"}, {"web_search_enabled": true},
		{"knowledge_bases": []any{"kb-2"}, "kb_selection_mode": "selected"}, {"agent_mode": "anything"}, {"model_id": 17},
	} {
		srv, agents, _, ctx := workspaceFixture()
		result := workspaceCall(t, srv, ctx, map[string]any{"action": "create", "name": "New", "config": config})
		require.True(t, result.IsError, "%v", config)
		require.Zero(t, agents.writes)
	}
}

func TestAgentWorkspaceOwnershipBuiltinsAndForeignIDs(t *testing.T) {
	srv, agents, ep, ctx := workspaceFixture()
	for _, args := range []map[string]any{
		{"action": "update", "agent_id": types.BuiltinQuickAnswerID, "name": "Changed"},
		{"action": "delete", "agent_id": types.BuiltinQuickAnswerID},
		{"action": "get", "agent_id": "foreign"},
	} {
		require.True(t, workspaceCall(t, srv, ctx, args).IsError)
	}
	other := webUserCallContext(1, ep, types.TenantRoleContributor, "other")
	require.True(t, workspaceCall(t, srv, other, map[string]any{"action": "delete", "agent_id": "own"}).IsError)
	require.Zero(t, agents.writes)
	admin := webUserCallContext(1, ep, types.TenantRoleAdmin, "admin")
	require.False(t, workspaceCall(t, srv, admin, map[string]any{"action": "delete", "agent_id": "own"}).IsError)
}

func TestCoreAskExplicitAgentIsAuthorizedAndActuallyExecutes(t *testing.T) {
	srv, _, ep, ctx := workspaceFixture()
	ep.DefaultAgentID = types.BuiltinQuickAnswerID
	result, err := srv.handleCoreAsk(ctx, legacyRequest(map[string]any{"question": "q", "agent_id": "own", "knowledge_base_ids": []string{"kb-1"}}))
	require.NoError(t, err)
	require.False(t, result.IsError, mcpErrorText(result))
	require.Equal(t, "own", structuredOf(result)["agent_id"])
	require.Equal(t, types.AgentModeQuickAnswer, structuredOf(result)["mode"], "explicit agent selection must report the agent's actual mode")
}

func TestCoreAskExplicitAgentModeChangeRefusesSessionReuse(t *testing.T) {
	srv, agents, ep, ctx := workspaceFixture()
	result, err := srv.handleCoreAsk(ctx, legacyRequest(map[string]any{"question": "q", "agent_id": "own"}))
	require.NoError(t, err)
	require.False(t, result.IsError, mcpErrorText(result))
	sessionID := structuredOf(result)["session_id"].(string)
	agents.rows["own"].Config.AgentMode = types.AgentModeSmartReasoning
	plan, err := srv.resolveAskPlan(ctx, ep, map[string]any{"agent_id": "own"})
	require.NoError(t, err)
	_, err = srv.validateAskSessionBinding(ctx, ep, sessionID, plan)
	require.Error(t, err, "changing the same agent's actual mode requires a new session")
}

func TestCoreAskExplicitAgentPermissionIdentityAndChoiceGuards(t *testing.T) {
	for _, scenario := range []string{"permission", "machine", "mode", "profile", "foreign"} {
		t.Run(scenario, func(t *testing.T) {
			srv, _, ep, ctx := workspaceFixture()
			args := map[string]any{"question": "q", "agent_id": "own"}
			switch scenario {
			case "permission":
				ep.Tools = []string{types.MCPEndpointToolAsk}
			case "machine":
				ctx = mcpCallContext(1, ep)
			case "mode":
				args["mode"] = "quick-answer"
			case "profile":
				ep.DefaultAgentID = "own"
				args["profile_id"] = "own"
			case "foreign":
				args["agent_id"] = "foreign"
			}
			_, err := srv.resolveAskPlan(ctx, ep, args)
			require.Error(t, err)
		})
	}
}

func TestAgentWorkspaceRejectsUnknownAndWrongTypedArgumentsBeforeWrite(t *testing.T) {
	for _, args := range []map[string]any{
		{"action": "create", "name": "New", "tenant_id": 2},
		{"action": "create", "name": "New", "config": "bad"},
		{"action": "create", "name": "New", "config": map[string]any{"knowledge_bases": "kb-1"}},
		{"action": "create", "name": "New", "config": map[string]any{"retrieve_kb_only_when_mentioned": "false"}},
		{"action": "update", "agent_id": "own", "name": false},
	} {
		srv, agents, _, ctx := workspaceFixture()
		require.True(t, workspaceCall(t, srv, ctx, args).IsError, "%v", args)
		require.Zero(t, agents.writes)
	}
}

func TestCoreAskExplicitAgentScopeAndSessionBinding(t *testing.T) {
	srv, _, ep, ctx := workspaceFixture()
	result, err := srv.handleCoreAsk(ctx, legacyRequest(map[string]any{"question": "q", "agent_id": "own", "knowledge_base_ids": []string{"kb-1"}}))
	require.NoError(t, err)
	require.False(t, result.IsError, mcpErrorText(result))
	sessionID := structuredOf(result)["session_id"].(string)
	result, err = srv.handleCoreAsk(ctx, legacyRequest(map[string]any{"question": "q", "agent_id": types.BuiltinQuickAnswerID, "session_id": sessionID}))
	require.NoError(t, err)
	require.True(t, result.IsError)
	_, err = srv.resolveAskPlan(ctx, ep, map[string]any{"agent_id": "own", "knowledge_base_ids": []string{"kb-2"}})
	require.Error(t, err)
}

func TestCoreAskExplicitAgentHonorsNativeDefaultKnowledgeScope(t *testing.T) {
	for _, scenario := range []string{"none", "selected-empty", "selected-intersection", "mention-only", "explicit-override"} {
		t.Run(scenario, func(t *testing.T) {
			srv, agents, ep, ctx := workspaceFixture()
			ep.KnowledgeBaseIDs = []string{"kb-1", "kb-2"}
			ctx = types.WithTenantAPIKeyScope(ctx, types.MCPEndpointScope(ep))
			a := agents.rows["own"]
			args := map[string]any{"agent_id": "own"}
			want := []string{}
			switch scenario {
			case "none":
				a.Config.KBSelectionMode = "none"
			case "selected-empty":
				a.Config.KnowledgeBases = nil
			case "selected-intersection":
				a.Config.KnowledgeBases = []string{"kb-1", "unavailable"}
				want = []string{"kb-1"}
			case "mention-only":
				a.Config.RetrieveKBOnlyWhenMentioned = true
			case "explicit-override":
				a.Config.KBSelectionMode = "none"
				args["knowledge_base_ids"] = []string{"kb-2"}
				want = []string{"kb-2"}
			}
			plan, err := srv.resolveAskPlan(ctx, ep, args)
			require.NoError(t, err)
			require.ElementsMatch(t, want, plan.KBIDs)
			require.True(t, plan.kbScopeKnown)
			if len(want) == 0 {
				require.Equal(t, "none", plan.Agent.Config.KBSelectionMode, "empty native runtime scope cannot fall back to the original agent's defaults")
			}
		})
	}
}
