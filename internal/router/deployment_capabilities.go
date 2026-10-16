package router

import (
	"github.com/Tencent/WeKnora/internal/handler"
	"github.com/Tencent/WeKnora/internal/sandbox"
)

func deploymentCapabilitiesFromRouter(params RouterParams) handler.DeploymentCapabilitiesData {
	// The knowledge-core profile does not register the non-core route groups,
	// so their capabilities must read as absent no matter which handlers the
	// container still builds. Reporting them as available would put menus and
	// settings in the SPA for routes that answer 404.
	coreProfile := params.Config != nil && params.Config.IsKnowledgeCore()

	return handler.BuildDeploymentCapabilities(handler.Edition, handler.DeploymentFeatureAvailability{
		Organizations: params.OrganizationHandler != nil,
		Agents:        params.CustomAgentHandler != nil,
		IM:            !coreProfile && params.IMHandler != nil,
		// Match RegisterEmbedChannelRoutes: management routes depend on handler only.
		Embed: !coreProfile && params.EmbedChannelHandler != nil,
		// Match RegisterMCPEndpointRoutes / RegisterMCPServerRoutes. The MCP
		// SERVER surface (the main product's entry point) stays in both
		// profiles; only the third-party MCP toolbox below is core-gated.
		MCPServer: params.MCPEndpointHandler != nil && params.MCPServer != nil && params.MCPEndpointService != nil,
		API:       params.TenantHandler != nil && params.TenantAPIKeyService != nil,
		MCP: !coreProfile && params.MCPServiceHandler != nil &&
			params.MCPCredentialsHandler != nil &&
			params.MCPOAuthHandler != nil,
		WebSearch: !coreProfile && params.WebSearchHandler != nil &&
			params.WebSearchProviderHandler != nil &&
			params.WebSearchCredentialsHandler != nil,
		VectorStore:   params.VectorStoreHandler != nil,
		Storage:       params.StorageBackendHandler != nil,
		Sandbox:       !coreProfile && params.SandboxConfigHandler != nil,
		SandboxDocker: !coreProfile && sandbox.DockerBackendEnabled(),
		SandboxHost:   !coreProfile && params.HostSandbox.Manager != nil,
		SandboxRemote: !coreProfile && params.SandboxConfigHandler != nil && !params.HostSandbox.Desktop,
	})
}
