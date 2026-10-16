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

// The knowledge-core profile is a security posture, so the negative test comes
// first: with the profile on, every non-core route group must read as closed,
// and the knowledge surface (knowledge base, parsing, MCP server, QA, wiki,
// graph) must stay open.
func TestKnowledgeCoreReachabilityClosesNonCoreRoutes(t *testing.T) {
	full := reachabilityFor(&config.Config{})
	if !full.PublicWebSearch ||
		!full.MCPClientToolbox || !full.ExecutionSandbox || !full.Skills {
		t.Fatalf("the default profile must keep every remaining route group: %+v", full)
	}

	core := reachabilityFor(&config.Config{ProductProfile: config.ProductProfileKnowledgeCore})
	if core.PublicWebSearch ||
		core.MCPClientToolbox || core.ExecutionSandbox || core.Skills {
		t.Fatalf("knowledge-core must close every remaining non-core route group: %+v", core)
	}

	// An unknown value must not be interpreted as "some other profile": config
	// normalization already maps it to full before the router sees it, and a
	// hand-built config with a typo falls back the same way.
	typo := reachabilityFor(&config.Config{ProductProfile: "knowledge_core"})
	if typo != full {
		t.Fatalf("an unknown profile must behave like full: %+v", typo)
	}

	// A nil config (lite/degraded construction paths) must not panic and must
	// behave like full.
	if reachabilityFor(nil) != full {
		t.Fatal("a nil config must behave like full")
	}
}

// The capability snapshot drives the SPA menus. If the knowledge-core profile
// left the non-core capabilities reported as supported while their routes are
// not registered, the UI would offer settings pages that answer 404.
func TestKnowledgeCoreCapabilitiesReportNonCoreAbsent(t *testing.T) {
	params := RouterParams{
		Config:                      &config.Config{ProductProfile: config.ProductProfileKnowledgeCore},
		OrganizationHandler:         &handler.OrganizationHandler{},
		CustomAgentHandler:          &handler.CustomAgentHandler{},
		TenantHandler:               &handler.TenantHandler{},
		VectorStoreHandler:          &handler.VectorStoreHandler{},
		StorageBackendHandler:       &handler.StorageBackendHandler{},
		MCPEndpointHandler:          &handler.MCPEndpointHandler{},
		MCPServer:                   &mcpserver.Server{},
		MCPEndpointService:          &stubEndpointService{},
		MCPServiceHandler:           &handler.MCPServiceHandler{},
		MCPCredentialsHandler:       &handler.MCPCredentialsHandler{},
		MCPOAuthHandler:             &handler.MCPOAuthHandler{},
		WebSearchHandler:            &handler.WebSearchHandler{},
		WebSearchProviderHandler:    &handler.WebSearchProviderHandler{},
		WebSearchCredentialsHandler: &handler.WebSearchProviderCredentialsHandler{},
		SandboxConfigHandler:        &handler.SandboxConfigHandler{},
		MessageSuggestionHandler:    &handler.MessageSuggestionHandler{},
		TenantMemberHandler:         &handler.TenantMemberHandler{},
		TenantInvitationHandler:     &handler.TenantInvitationHandler{},
		AuditLogHandler:             &handler.AuditLogHandler{},
	}

	caps := deploymentCapabilitiesFromRouter(params).Capabilities
	// IM and embed are physically removed, so they read as unsupported in every
	// profile; the rest are still only closed by the profile switch.
	for _, key := range []string{
		"integrations.im", "integrations.embed", "settings.mcp", "settings.websearch", "settings.sandbox",
	} {
		if caps[key].Supported {
			t.Fatalf("knowledge-core must not report %s as supported: %+v", key, caps[key])
		}
	}
	// The knowledge surface and the MCP SERVER entry point stay available even
	// though the MCP client toolbox is closed.
	for _, key := range []string{"organizations", "agents", "integrations.mcpserver", "settings.vectorstore", "settings.storage"} {
		if !caps[key].Supported {
			t.Fatalf("knowledge-core must keep %s available: %+v", key, caps[key])
		}
	}

	// Under the default profile the still-present features come back, so the
	// closures above are deliberate and not missing wiring. IM and embed do not
	// come back in any profile: they are deleted, not gated.
	params.Config = &config.Config{}
	fullCaps := deploymentCapabilitiesFromRouter(params).Capabilities
	for _, key := range []string{"settings.mcp", "settings.websearch", "settings.sandbox"} {
		if !fullCaps[key].Supported {
			t.Fatalf("the full profile must keep %s available: %+v", key, fullCaps[key])
		}
	}
	for _, key := range []string{"integrations.im", "integrations.embed"} {
		if fullCaps[key].Supported {
			t.Fatalf("%s is removed from the build and must not be reported as supported: %+v", key, fullCaps[key])
		}
	}
}

// ProductProfile defaults and env handling: the profile must never be empty in
// a running process, and an unknown value must resolve to full rather than to a
// profile that does not exist.
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
	if (&config.Config{ProductProfile: config.ProductProfileKnowledgeCore}).IsKnowledgeCore() != true {
		t.Fatal("IsKnowledgeCore must report true for the knowledge-core profile")
	}
	if (&config.Config{}).IsKnowledgeCore() {
		t.Fatal("the zero-value config must not be knowledge-core")
	}
}
