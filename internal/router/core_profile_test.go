package router

import (
	"testing"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/handler"
	"github.com/Tencent/WeKnora/internal/mcpserver"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// stubEndpointService satisfies the dependency only to make the capability
// snapshot report the MCP SERVER as wired; no method is called.
type stubEndpointService struct{ interfaces.MCPEndpointService }

// TestRemovedModulesReadAsUnsupported pins the capability snapshot: every module
// the pruning batches deleted (IM, embed, public web search, the execution
// sandbox and the third-party MCP client toolbox) must read unsupported in
// BOTH profiles — deleted, not gated — while the knowledge surface and the MCP
// SERVER entry point stay wired.
func TestRemovedModulesReadAsUnsupported(t *testing.T) {
	params := RouterParams{
		Config:                      &config.Config{},
		OrganizationHandler:         &handler.OrganizationHandler{},
		CustomAgentHandler:          &handler.CustomAgentHandler{},
		TenantHandler:               &handler.TenantHandler{},
		VectorStoreHandler:          &handler.VectorStoreHandler{},
		StorageBackendHandler:       &handler.StorageBackendHandler{},
		MCPEndpointHandler:          &handler.MCPEndpointHandler{},
		MCPServer:                   &mcpserver.Server{},
		MCPEndpointService:          &stubEndpointService{},
		MessageSuggestionHandler:    &handler.MessageSuggestionHandler{},
		TenantMemberHandler:         &handler.TenantMemberHandler{},
		TenantInvitationHandler:     &handler.TenantInvitationHandler{},
		AuditLogHandler:             &handler.AuditLogHandler{},
		TenantAPIKeyService:         nil,
	}

	removed := []string{
		"integrations.im", "integrations.embed", "settings.websearch", "settings.mcp",
		"settings.sandbox", "settings.sandbox.docker", "settings.sandbox.host", "settings.sandbox.remote",
	}
	kept := []string{"organizations", "agents", "integrations.mcpserver", "settings.vectorstore", "settings.storage"}

	for _, profile := range []string{config.ProductProfileFull, config.ProductProfileKnowledgeCore} {
		params.Config = &config.Config{ProductProfile: profile}
		caps := deploymentCapabilitiesFromRouter(params).Capabilities
		for _, key := range removed {
			if caps[key].Supported {
				t.Fatalf("profile %q must not report removed %s as supported: %+v", profile, key, caps[key])
			}
		}
		for _, key := range kept {
			if !caps[key].Supported {
				t.Fatalf("profile %q must keep %s available: %+v", profile, key, caps[key])
			}
		}
	}
}

// ProductProfile defaults and env handling: the profile must never be empty in
// a running process, and an unknown value must resolve to full rather than to a
// profile that does not exist. Since every non-core module was physically
// removed, the profile no longer gates route registration; what remains is the
// agent-side tool filter (see tools.FilterToolsForCoreProfile).
func TestProductProfileNormalization(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", config.ProductProfileFull},
		{"full", config.ProductProfileFull},
		{"knowledge-core", config.ProductProfileKnowledgeCore},
		{"  knowledge-core  ", config.ProductProfileKnowledgeCore},
		{"KNOWLEDGE-CORE", config.ProductProfileKnowledgeCore},
		{"knowledge_core", config.ProductProfileFull},
	}
	for _, c := range cases {
		if got := config.NormalizeProductProfileForTest(c.in); got != c.want {
			t.Fatalf("profile %q normalized to %q, want %q", c.in, got, c.want)
		}
	}
	if !(&config.Config{ProductProfile: config.ProductProfileKnowledgeCore}).IsKnowledgeCore() {
		t.Fatal("IsKnowledgeCore must report true for the knowledge-core profile")
	}
	if (&config.Config{}).IsKnowledgeCore() {
		t.Fatal("the zero-value config must not be knowledge-core")
	}
}
