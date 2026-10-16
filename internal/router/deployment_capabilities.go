package router

import (
	"github.com/Tencent/WeKnora/internal/handler"
	"github.com/Tencent/WeKnora/internal/sandbox"
)

func deploymentCapabilitiesFromRouter(params RouterParams) handler.DeploymentCapabilitiesData {
	reach := reachabilityFor(params.Config)

	// IM delivery and web-embed publishing were physically removed from this
	// build, and public web search joined them in the web-search deletion
	// batch, so those capabilities read as unsupported in every profile. The
	// other non-core features are still only gated by the product profile, so a
	// capability follows both the profile switch and the wiring: reporting a
	// gated feature as supported would put menus and settings in the SPA for
	// routes this process never registers.
	return handler.BuildDeploymentCapabilities(handler.Edition, handler.DeploymentFeatureAvailability{
		Organizations: params.OrganizationHandler != nil,
		Agents:        params.CustomAgentHandler != nil,
		IM:            false,
		Embed:         false,
		// Match RegisterMCPEndpointRoutes / RegisterMCPServerRoutes. The MCP
		// SERVER surface — the main product's entry point — stays in every
		// profile; only the third-party MCP client toolbox below is gated.
		MCPServer: params.MCPEndpointHandler != nil && params.MCPServer != nil && params.MCPEndpointService != nil,
		API:       params.TenantHandler != nil && params.TenantAPIKeyService != nil,
		MCP: reach.MCPClientToolbox && params.MCPServiceHandler != nil &&
			params.MCPCredentialsHandler != nil &&
			params.MCPOAuthHandler != nil,
		WebSearch:     false,
		VectorStore:   params.VectorStoreHandler != nil,
		Storage:       params.StorageBackendHandler != nil,
		Sandbox:       reach.ExecutionSandbox && params.SandboxConfigHandler != nil,
		SandboxDocker: reach.ExecutionSandbox && sandbox.DockerBackendEnabled(),
		SandboxHost:   reach.ExecutionSandbox && params.HostSandbox.Manager != nil,
		SandboxRemote: reach.ExecutionSandbox && params.SandboxConfigHandler != nil && !params.HostSandbox.Desktop,
	})
}
