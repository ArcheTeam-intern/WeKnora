package router

import (
	"github.com/Tencent/WeKnora/internal/handler"
)

func deploymentCapabilitiesFromRouter(params RouterParams) handler.DeploymentCapabilitiesData {

	// IM delivery, web-embed publishing, public web search, the agent
	// execution sandbox (terminal, desktop, local browser, sandbox/skill
	// settings) and the third-party MCP client toolbox were physically removed
	// from this build, so those capabilities read as unsupported in every
	// profile. What remains follows the wiring: reporting a missing feature as
	// supported would put menus and settings in the SPA for routes this process
	// never registers.
	return handler.BuildDeploymentCapabilities(handler.Edition, handler.DeploymentFeatureAvailability{
		Organizations: params.OrganizationHandler != nil,
		Agents:        params.CustomAgentHandler != nil,
		IM:            false,
		Embed:         false,
		// Match RegisterMCPEndpointRoutes / RegisterMCPServerRoutes. The MCP
		// SERVER surface — the main product's entry point — stays in every
		// profile.
		MCPServer:     params.MCPEndpointHandler != nil && params.MCPServer != nil && params.MCPEndpointService != nil,
		API:           params.TenantHandler != nil && params.TenantAPIKeyService != nil,
		MCP:           false,
		WebSearch:     false,
		VectorStore:   params.VectorStoreHandler != nil,
		Storage:       params.StorageBackendHandler != nil,
		Sandbox:       false,
		SandboxDocker: false,
		SandboxHost:   false,
		SandboxRemote: false,
	})
}
