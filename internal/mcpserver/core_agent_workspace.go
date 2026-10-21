package mcpserver

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/mark3labs/mcp-go/mcp"
)

func coreAgentWorkspaceTool() mcp.Tool {
	return mcp.NewTool(CoreToolAgentWorkspace,
		mcp.WithDescription("List, inspect, create, update, delete or copy native knowledge QA agents. Requires explicit agent_workspace permission and a signed-in workspace user. Config edits merge only knowledge QA fields; built-in agents may be copied but not edited or deleted. Use ask.agent_id to run a visible agent within this endpoint's library scope."),
		mcp.WithString("action", mcp.Required(), mcp.Enum("list", "get", "create", "update", "delete", "copy")),
		mcp.WithString("agent_id", mcp.Description("Required for get, update, delete and copy")),
		mcp.WithString("name", mcp.Description("Required for create; optional new name for update or copy")),
		mcp.WithString("description"),
		mcp.WithObject("config", mcp.Description("Partial config: agent_mode, agent_type, system_prompt, model_id, rerank_model_id, kb_selection_mode, knowledge_bases (string array), retrieve_kb_only_when_mentioned (boolean). No tools, credentials, network, skills or sandbox fields.")),
		mcp.WithReadOnlyHintAnnotation(false), mcp.WithDestructiveHintAnnotation(true))
}

// Explicit DTOs prevent future native configuration fields from leaking into
// the MCP response (provider secrets and non-core capabilities are never DTOs).
type coreAgentConfig struct {
	AgentMode                   string   `json:"agent_mode"`
	AgentType                   string   `json:"agent_type"`
	SystemPrompt                string   `json:"system_prompt"`
	ModelID                     string   `json:"model_id"`
	RerankModelID               string   `json:"rerank_model_id"`
	KBSelectionMode             string   `json:"kb_selection_mode"`
	KnowledgeBases              []string `json:"knowledge_bases"`
	RetrieveKBOnlyWhenMentioned bool     `json:"retrieve_kb_only_when_mentioned"`
}
type coreAgentDTO struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	IsBuiltin   bool            `json:"is_builtin"`
	CreatedBy   string          `json:"created_by"`
	Config      coreAgentConfig `json:"config"`
}

func agentDTO(a *types.CustomAgent) coreAgentDTO {
	c := a.Config
	return coreAgentDTO{a.ID, a.Name, a.Description, a.IsBuiltin, a.CreatedBy,
		coreAgentConfig{c.AgentMode, c.AgentType, c.SystemPrompt, c.ModelID, c.RerankModelID, c.KBSelectionMode, append([]string{}, c.KnowledgeBases...), c.RetrieveKBOnlyWhenMentioned}}
}

func requireAgentWorkspaceUser(ctx context.Context, ep *types.MCPEndpoint) (context.Context, error) {
	if !ep.HasTool(types.MCPEndpointToolAgentWorkspace) {
		return ctx, corePermissionError(CoreToolAgentWorkspace, CoreActionSingle, []string{types.MCPEndpointToolAgentWorkspace})
	}
	p, ok := types.PrincipalFromContext(ctx)
	c := types.CallerFromContext(ctx)
	if !ok || p.Type != types.PrincipalWebUser || c.UserID == "" || p.ID != c.UserID || types.IsSyntheticUserID(c.UserID) || c.TenantID != ep.TenantID {
		return ctx, forbidden("agent workspace and explicit agent selection require a signed-in workspace user")
	}
	return context.WithValue(ctx, types.UserIDContextKey, c.UserID), nil
}

func (s *Server) visibleWorkspaceAgent(ctx context.Context, ep *types.MCPEndpoint, id string) (*types.CustomAgent, error) {
	if s.agentService == nil {
		return nil, &ToolError{Code: CodeUpstreamUnavailable, Message: "native agent service is unavailable"}
	}
	a, err := s.agentService.GetAgentByID(ctx, id)
	if err != nil || !types.MCPEndpointAgentAllowed(a, ep.TenantID) {
		return nil, notFound("agent is not visible in this workspace")
	}
	return a, nil
}

func cloneNativeAgent(a *types.CustomAgent) (*types.CustomAgent, error) {
	data, err := json.Marshal(a)
	if err != nil {
		return nil, err
	}
	var out types.CustomAgent
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Selection grants knowledge QA, never the selected agent's unrelated tools.
// Clone before narrowing so stored native settings and other callers survive.
func knowledgeOnlyAgent(a *types.CustomAgent) (*types.CustomAgent, error) {
	out, err := cloneNativeAgent(a)
	if err != nil {
		return nil, err
	}
	c := &out.Config
	c.WebSearchEnabled = false
	c.MCPSelectionMode, c.MCPServices = "none", nil
	c.SkillsSelectionMode, c.SelectedSkills = "none", nil
	c.SandboxConfigID = ""
	disabled := false
	c.MemoryEnabled = &disabled
	var allowed []string
	for _, name := range tools.FilterToolsForCoreProfile(c.AllowedTools) {
		switch name {
		case tools.ToolSearchKnowledge, tools.ToolReadDocument, tools.ToolListDocuments, tools.ToolQueryKnowledgeGraph,
			tools.ToolWikiSearch, tools.ToolWikiReadPage, tools.ToolThinking, tools.ToolTodoWrite, tools.ToolDataSchema, tools.ToolDataAnalysis:
			allowed = append(allowed, name)
		}
	}
	if len(c.AllowedTools) == 0 {
		allowed = []string{tools.ToolSearchKnowledge, tools.ToolReadDocument, tools.ToolListDocuments, tools.ToolQueryKnowledgeGraph, tools.ToolWikiSearch, tools.ToolWikiReadPage}
	} else if len(allowed) == 0 {
		return nil, invalidArgument("this agent has no usable knowledge QA tools; select or create a knowledge agent")
	}
	c.AllowedTools = allowed
	return out, nil
}

func (s *Server) handleCoreAgentWorkspace(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := coreArgs(req)
	ep, err := endpointFromContext(ctx)
	if err != nil {
		return errorResult(&ToolError{Code: CodeUnauthorized, Message: "unauthorized"}), nil
	}
	ctx, err = requireAgentWorkspaceUser(ctx, ep)
	if err != nil {
		return errorResult(err), nil
	}
	for key := range args {
		switch key {
		case "action", "agent_id", "name", "description", "config":
		default:
			return errorResult(invalidArgument("unsupported agent workspace argument %q", key)), nil
		}
	}
	action, err := coreStringArg(args, "action", "")
	if err != nil {
		return errorResult(err), nil
	}
	if _, err := resolveCoreAction(CoreToolAgentWorkspace, action, args); err != nil {
		return errorResult(err), nil
	}
	for key, raw := range args {
		allowed := key == "action" || key == "agent_id" && action != CoreActionList && action != CoreActionCreate || key == "name" && (action == CoreActionCreate || action == CoreActionUpdate || action == CoreActionCopy) || (key == "description" || key == "config") && (action == CoreActionCreate || action == CoreActionUpdate)
		if !allowed {
			return errorResult(invalidArgument("argument %q is not supported for %s", key, action)), nil
		}
		if key == "config" {
			if _, ok := raw.(map[string]any); !ok {
				return errorResult(invalidArgument("config must be an object")), nil
			}
		} else if _, ok := raw.(string); !ok {
			return errorResult(invalidArgument("%s must be a string", key)), nil
		}
	}
	if s.agentService == nil {
		return errorResult(&ToolError{Code: CodeUpstreamUnavailable, Message: "native agent service is unavailable"}), nil
	}
	if action == CoreActionList {
		agents, err := s.agentService.ListAgents(ctx)
		if err != nil {
			return errorResult(err), nil
		}
		rows := []coreAgentDTO{}
		for _, a := range agents {
			if types.MCPEndpointAgentAllowed(a, ep.TenantID) {
				rows = append(rows, agentDTO(a))
			}
		}
		return jsonResult(map[string]any{"agents": rows})
	}
	var a *types.CustomAgent
	if action != CoreActionCreate {
		id, err := coreStringArg(args, "agent_id", "")
		if err != nil || id == "" {
			return errorResult(invalidArgument("agent_id is required")), nil
		}
		a, err = s.visibleWorkspaceAgent(ctx, ep, id)
		if err != nil {
			return errorResult(err), nil
		}
		if action == CoreActionGet {
			return jsonResult(map[string]any{"agent": agentDTO(a)})
		}
	}
	caller := types.CallerFromContext(ctx)
	if !caller.Role.HasPermission(types.TenantRoleContributor) {
		return errorResult(forbidden("agent writes require contributor permission or higher")), nil
	}
	if action == CoreActionUpdate || action == CoreActionDelete {
		if a.IsBuiltin || types.IsBuiltinAgentID(a.ID) {
			return errorResult(forbidden("built-in agents cannot be edited or deleted; copy the agent first")), nil
		}
		if !caller.Role.HasPermission(types.TenantRoleAdmin) && (a.CreatedBy == "" || a.CreatedBy != caller.UserID) {
			return errorResult(forbidden("agent writes require its creator or a workspace administrator")), nil
		}
	}
	if action == CoreActionDelete {
		if err := s.agentService.DeleteAgent(ctx, a.ID); err != nil {
			return errorResult(err), nil
		}
		return jsonResult(map[string]any{"deleted": true})
	}
	name, err := coreStringArg(args, "name", "")
	if err != nil {
		return errorResult(err), nil
	}
	if _, ok := args["name"]; ok && name == "" || len([]rune(name)) > 255 || action == CoreActionCreate && name == "" {
		return errorResult(invalidArgument("name must contain between 1 and 255 characters")), nil
	}
	description, err := coreStringArg(args, "description", "")
	if err != nil {
		return errorResult(err), nil
	}
	if action == CoreActionCopy {
		// Validate overrides and scope before creating a native copy.
		if _, ok := args["config"]; ok {
			return errorResult(invalidArgument("copy accepts agent_id and an optional name; edit config after copying")), nil
		}
		if _, ok := args["description"]; ok {
			return errorResult(invalidArgument("copy accepts agent_id and an optional name")), nil
		}
		copyConfig, err := s.validateWorkspaceAgentScope(ctx, ep, a.Config)
		if err != nil {
			return errorResult(err), nil
		}
		copied, err := s.agentService.CopyAgent(ctx, a.ID)
		if err != nil {
			return errorResult(err), nil
		}
		if name != "" {
			copied.Name = name
		}
		copied.Config = copyConfig
		copied, err = s.agentService.UpdateAgent(ctx, copied, nil)
		if err != nil {
			return errorResult(err), nil
		}
		return jsonResult(map[string]any{"agent": agentDTO(copied)})
	}
	if action == CoreActionCreate {
		mode := types.AgentModeQuickAnswer
		if raw, ok := args["config"].(map[string]any); ok {
			if value, ok := raw["agent_mode"].(string); ok && value != "" {
				mode = value
			}
		}
		seedID := types.BuiltinQuickAnswerID
		if mode == types.AgentModeSmartReasoning {
			seedID = types.BuiltinSmartReasoningID
		}
		seed, seedErr := s.agentService.GetAgentByID(ctx, seedID)
		if seedErr != nil || seed == nil {
			seed = &types.CustomAgent{Config: types.CustomAgentConfig{AgentMode: mode, KBSelectionMode: "all"}}
		}
		a, err = knowledgeOnlyAgent(seed)
		if err != nil {
			return errorResult(err), nil
		}
		a.ID, a.TenantID, a.CreatedBy, a.IsBuiltin = "", ep.TenantID, caller.UserID, false
		a.Name, a.Description = name, description
		s.workspaceAgentModelDefaults(ctx, &a.Config)
	} else {
		a, err = cloneNativeAgent(a)
		if err != nil {
			return errorResult(err), nil
		}
		if _, ok := args["name"]; ok {
			a.Name = name
		}
		if _, ok := args["description"]; ok {
			a.Description = description
		}
	}
	if err := mergeWorkspaceAgentConfig(&a.Config, args["config"]); err != nil {
		return errorResult(err), nil
	}
	s.workspaceAgentModelDefaults(ctx, &a.Config)
	a.Config, err = s.validateWorkspaceAgentScope(ctx, ep, a.Config)
	if err != nil {
		return errorResult(err), nil
	}
	if err := s.validateWorkspaceAgentModels(ctx, ep, a.Config); err != nil {
		return errorResult(err), nil
	}
	if action == CoreActionCreate {
		a, err = s.agentService.CreateAgent(ctx, a)
	} else {
		a, err = s.agentService.UpdateAgent(ctx, a, nil)
	}
	if err != nil {
		return errorResult(err), nil
	}
	return jsonResult(map[string]any{"agent": agentDTO(a)})
}

func mergeWorkspaceAgentConfig(config *types.CustomAgentConfig, raw any) error {
	if raw == nil {
		return nil
	}
	patch, ok := raw.(map[string]any)
	if !ok {
		return invalidArgument("config must be an object")
	}
	for key, raw := range patch {
		switch key {
		case "knowledge_bases":
			ids := []string{}
			switch list := raw.(type) {
			case []any:
				for _, item := range list {
					id, ok := item.(string)
					if !ok {
						return invalidArgument("knowledge_bases must be a string array")
					}
					ids = append(ids, id)
				}
			case []string:
				ids = append(ids, list...)
			default:
				return invalidArgument("knowledge_bases must be a string array")
			}
			config.KnowledgeBases = sortDedupStrings(ids)
		case "retrieve_kb_only_when_mentioned":
			value, ok := raw.(bool)
			if !ok {
				return invalidArgument("retrieve_kb_only_when_mentioned must be a boolean")
			}
			config.RetrieveKBOnlyWhenMentioned = value
		case "agent_mode", "agent_type", "system_prompt", "model_id", "rerank_model_id", "kb_selection_mode":
			value, ok := raw.(string)
			if !ok {
				return invalidArgument("%s must be a string", key)
			}
			switch key {
			case "agent_mode":
				config.AgentMode = value
			case "agent_type":
				config.AgentType = value
			case "system_prompt":
				config.SystemPrompt = value
			case "model_id":
				config.ModelID = strings.TrimSpace(value)
			case "rerank_model_id":
				config.RerankModelID = strings.TrimSpace(value)
			case "kb_selection_mode":
				config.KBSelectionMode = value
			}
		default:
			return invalidArgument("config field %q is not part of knowledge QA", key)
		}
	}
	if config.AgentMode != types.AgentModeQuickAnswer && config.AgentMode != types.AgentModeSmartReasoning {
		return invalidArgument("agent_mode must be quick-answer or smart-reasoning")
	}
	switch config.AgentType {
	case "", types.AgentTypeRAGQA, types.AgentTypeWikiQA, types.AgentTypeHybridRAGWiki, types.AgentTypeCustom:
	default:
		return invalidArgument("agent_type must be rag-qa, wiki-qa, hybrid-rag-wiki or custom")
	}
	switch config.KBSelectionMode {
	case "", "all", "selected", "none":
	default:
		return invalidArgument("kb_selection_mode must be all, selected or none")
	}
	return nil
}

func (s *Server) validateWorkspaceAgentScope(ctx context.Context, ep *types.MCPEndpoint, config types.CustomAgentConfig) (types.CustomAgentConfig, error) {
	if len(config.KnowledgeBases) > 0 {
		ids, err := s.resolveAskKnowledgeBases(ctx, ep, &types.CustomAgent{Config: config}, config.KnowledgeBases)
		if err != nil {
			return config, invalidArgument("agent knowledge_bases are outside the authorized endpoint scope")
		}
		config.KnowledgeBases = sortDedupStrings(ids)
	}
	if (config.KBSelectionMode == "all" || config.KBSelectionMode == "") && (ep.RestrictsKnowledgeBases() || RequestScopeOf(ctx).Narrows()) {
		ids, err := s.resolveAskKnowledgeBases(ctx, ep, &types.CustomAgent{Config: config}, nil)
		if err != nil {
			return config, err
		}
		config.KBSelectionMode, config.KnowledgeBases = "selected", sortDedupStrings(ids)
	}
	return config, nil
}

func (s *Server) workspaceAgentModelDefaults(ctx context.Context, config *types.CustomAgentConfig) {
	if s.db == nil {
		return
	}
	tenantID := types.MustTenantIDFromContext(ctx)
	for _, item := range []struct {
		target *string
		kind   types.ModelType
	}{{&config.ModelID, types.ModelTypeKnowledgeQA}, {&config.RerankModelID, types.ModelTypeRerank}} {
		if *item.target != "" {
			continue
		}
		var id string
		if s.db.WithContext(ctx).Table("models").Select("id").Where("tenant_id = ? AND type = ? AND deleted_at IS NULL", tenantID, string(item.kind)).Order("is_default DESC, created_at ASC").Limit(1).Scan(&id).Error == nil {
			*item.target = id
		}
	}
}

func (s *Server) validateWorkspaceAgentModels(ctx context.Context, ep *types.MCPEndpoint, config types.CustomAgentConfig) error {
	if s.db == nil {
		return nil
	}
	if config.ModelID == "" {
		return invalidArgument("configure a workspace KnowledgeQA model before creating or updating this agent")
	}
	for _, item := range []struct {
		id   string
		kind types.ModelType
	}{{config.ModelID, types.ModelTypeKnowledgeQA}, {config.RerankModelID, types.ModelTypeRerank}} {
		if item.id == "" {
			continue
		}
		var count int64
		err := s.db.WithContext(ctx).Table("models").Where("id = ? AND tenant_id = ? AND type = ? AND deleted_at IS NULL", item.id, ep.TenantID, string(item.kind)).Count(&count).Error
		if err != nil || count == 0 {
			return invalidArgument("configured model is not available in this workspace")
		}
	}
	return nil
}
