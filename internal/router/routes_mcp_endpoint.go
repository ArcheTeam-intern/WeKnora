package router

import (
	"net/http"

	"github.com/Tencent/WeKnora/internal/handler"
	"github.com/Tencent/WeKnora/internal/mcpserver"
	"github.com/Tencent/WeKnora/internal/middleware"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/gin-gonic/gin"
)

// RegisterMCPEndpointRoutes registers the management API for workspace MCP
// endpoints. Endpoints are tenant-wide integration surfaces like embed
// channels: reads for every member, mutations for admins, and API keys need
// the manage_channels capability.
func RegisterMCPEndpointRoutes(r *gin.RouterGroup, h *handler.MCPEndpointHandler, g *rbacGuards) {
	if h == nil {
		return
	}
	grp := g.apiKeyGroup(r.Group("/mcp-endpoints"), apiKeyManageChannels(apiKeyFullAccess()))
	grp.GET("", g.Viewer(), h.ListMCPEndpoints)
	grp.GET("/tools", g.Viewer(), h.ListToolCatalog)
	grp.POST("", g.Admin(), h.CreateMCPEndpoint)
	grp.GET("/:endpoint_id", g.Viewer(), h.GetMCPEndpoint)
	grp.PUT("/:endpoint_id", g.Admin(), h.UpdateMCPEndpoint)
	grp.DELETE("/:endpoint_id", g.Admin(), h.DeleteMCPEndpoint)
	grp.POST("/:endpoint_id/rotate-token", g.Admin(), h.RotateMCPEndpointToken)
}

// RegisterMCPServerRoutes mounts the public MCP server surface at
// /mcp/:endpoint_id. It carries its own bearer-token auth and is registered
// before the global Auth middleware, like the embed public routes.
//
// The identity dependencies are optional so callers that never publish
// user-scoped endpoints (and existing tests) keep working; when they are
// omitted, only endpoints in machine mode can be used.
func RegisterMCPServerRoutes(
	r *gin.Engine,
	srv *mcpserver.Server,
	endpointService interfaces.MCPEndpointService,
	tenantService interfaces.TenantService,
	identity ...middleware.MCPIdentityDeps,
) {
	if srv == nil || endpointService == nil {
		return
	}
	h := gin.WrapH(srv.Handler())
	auth := middleware.MCPEndpointAuth(endpointService, tenantService, identity...)
	for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodDelete} {
		r.Handle(method, handler.MCPEndpointPath+":endpoint_id", auth, h)
	}
	// The binary upload channel of the same integration service. It shares the
	// MCP authentication (endpoint token + user identity + request scope) and is
	// mounted here so both surfaces are reachable under one address. It only
	// stages bytes: creating knowledge stays a MCP tool call.
	r.POST(handler.MCPEndpointPath+":endpoint_id/uploads", auth, gin.WrapH(srv.HandleUpload()))
}
