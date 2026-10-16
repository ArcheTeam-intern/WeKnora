package router

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/Tencent/WeKnora/internal/handler"
)

// RegisterCustomAgentRoutes registers custom agent routes.
//
// Mutating routes use OwnedAgentOrAdmin: the original creator can edit
// their agent, otherwise Admin+ is required. Built-in agents
// (IsBuiltin=true) have an empty creator and are always Admin+. Reads
// are Viewer+, copy is Contributor+ (the copy is owned by the caller).
func RegisterCustomAgentRoutes(r *gin.RouterGroup, agentHandler *handler.CustomAgentHandler, g *rbacGuards) {
	agents := g.apiKeyGroup(r.Group("/agents"), apiKeyFullAccess())
	// agentsRead are the agent read endpoints. They stay full-access only for
	// plain scoped keys (agent config can carry sensitive model/MCP bindings),
	// but read_agents, chat, or manage_agents may read them.
	agentsRead := agents.With(apiKeyReadAgents(apiKeyManageAgents(apiKeyChat(apiKeyFullAccess()))))
	// agentsWrite are the agent authoring endpoints. Owner by default, but a
	// key granted manage_agents may author agents without full Owner.
	agentsWrite := agents.With(apiKeyManageAgents(apiKeyFullAccess()))
	{
		// Get placeholder definitions (must be before /:id to avoid conflict) — Viewer+
		agentsRead.GET("/placeholders", g.Viewer(), agentHandler.GetPlaceholders)
		// List smart-reasoning agent type presets (rag-qa / wiki-qa / hybrid / custom) — Viewer+
		agentsRead.GET("/type-presets", g.Viewer(), agentHandler.GetAgentTypePresets)
		// Create custom agent — Contributor+
		agentsWrite.POST("", g.Contributor(), agentHandler.CreateAgent)
		// List all agents (including built-in) — Viewer+
		agentsRead.GET("", g.Viewer(), agentHandler.ListAgents)
		// Get agent by ID — Viewer+
		agentsRead.GET("/:id", g.Viewer(), agentHandler.GetAgent)
		// Update agent — creator OR Admin+
		agentsWrite.PUT("/:id", g.OwnedAgentOrAdmin(), agentHandler.UpdateAgent)
		// Delete agent — creator OR Admin+
		agentsWrite.DELETE("/:id", g.OwnedAgentOrAdmin(), agentHandler.DeleteAgent)
		// Copy agent — Contributor+ (copy is owned by the caller)
		agentsWrite.POST("/:id/copy", g.Contributor(), agentHandler.CopyAgent)
	}
	// Registered outside the group to avoid Gin route conflict with /agents/:id/shares in organization routes
	g.apiKeyRoute(r, http.MethodGet, "/agents/:id/suggested-questions",
		apiKeyReadAgents(apiKeyManageAgents(apiKeyChat(apiKeyFullAccess()))), g.Viewer(), agentHandler.GetSuggestedQuestions)
}

// RegisterUserFavoriteRoutes wires the per-user starred-resource endpoints.
//
// Authorization: the handler always derives (user_id, tenant_id) from the
// auth context — there is no admin-style "see another user's favorites"
// path — so a Viewer floor is the right gate. The endpoints intentionally
// don't follow the OwnedXOrAdmin pattern: favorites aren't owned by the
// resource's creator, they're owned by the user *doing* the starring.
func RegisterUserFavoriteRoutes(r *gin.RouterGroup, h *handler.UserResourceFavoriteHandler, g *rbacGuards) {
	// Favorites are per-user; not declared for API keys (default-deny).
	favs := r.Group("/user/favorites")
	{
		favs.GET("", g.Viewer(), h.ListFavorites)
		favs.POST("", g.Viewer(), h.AddFavorite)
		favs.DELETE("/:type/:id", g.Viewer(), h.RemoveFavorite)
	}
}

// RegisterOrganizationRoutes registers organization and sharing routes
func RegisterOrganizationRoutes(r *gin.RouterGroup, orgHandler *handler.OrganizationHandler, g *rbacGuards) {
	// Organization routes
	orgs := g.apiKeyGroup(r.Group("/organizations"), apiKeyManageSpaces(apiKeyFullAccess()))
	{
		// Create organization (Admin+ in caller's tenant only)
		orgs.POST("", g.Admin(), orgHandler.CreateOrganization)
		// List my organizations — Viewer+ floor so revoked/non-member
		// accounts whose JWT still validates can't enumerate org membership.
		orgs.GET("", g.Viewer(), orgHandler.ListMyOrganizations)
		// Preview organization by invite code (without joining) — Viewer+
		orgs.GET("/preview/:code", g.Viewer(), orgHandler.PreviewByInviteCode)
		// Join organization by invite code (Admin+ in caller's tenant only)
		orgs.POST("/join", g.Admin(), orgHandler.JoinByInviteCode)
		// Submit join request (for organizations that require approval) (Admin+)
		orgs.POST("/join-request", g.Admin(), orgHandler.SubmitJoinRequest)
		// Search searchable (discoverable) organizations — Viewer+
		orgs.GET("/search", g.Viewer(), orgHandler.SearchOrganizations)
		// Join searchable organization by ID (no invite code) (Admin+)
		orgs.POST("/join-by-id", g.Admin(), orgHandler.JoinByOrganizationID)
		// Get organization by ID — Viewer+
		orgs.GET("/:id", g.Viewer(), orgHandler.GetOrganization)
		// Update organization — Admin+ in caller's tenant. The service
		// requires the caller's tenant to be an org admin (not only the
		// owner), matching the settings UI; the route guard stops a tenant
		// Viewer/Contributor from ever reaching the service.
		orgs.PUT("/:id", g.Admin(), orgHandler.UpdateOrganization)
		// Delete organization — Admin+ in caller's tenant; the service
		// further requires the org's owner tenant, since deletion is
		// irreversible.
		orgs.DELETE("/:id", g.Admin(), orgHandler.DeleteOrganization)
		// Leave organization (Admin+ in caller's tenant only)
		orgs.POST("/:id/leave", g.Admin(), orgHandler.LeaveOrganization)
		// Request role upgrade (Admin+ in caller's tenant only).
		// An upgrade approval changes the whole tenant's org role, so it
		// must not be initiated by a tenant Viewer/Contributor.
		orgs.POST("/:id/request-upgrade", g.Admin(), orgHandler.RequestRoleUpgrade)
		// Generate invite code — Admin+ in caller's tenant. Issuing an
		// invite code is an admin action; the service layer additionally
		// requires the caller's tenant to be admin in the org.
		orgs.POST("/:id/invite-code", g.Admin(), orgHandler.GenerateInviteCode)
		// Search tenants for invite (admin only). Plan 3 changed the
		// unit of membership to "tenant"; this endpoint returns
		// candidate tenants (with one representative user attached)
		// instead of one row per user.
		orgs.GET("/:id/search-tenants", g.Admin(), orgHandler.SearchTenantsForInvite)
		// Deprecated alias for /:id/search-tenants. Old frontends that
		// still hit search-users will receive the tenant-grouped shape;
		// the deprecation is documented in the handler.
		orgs.GET("/:id/search-users", g.Admin(), orgHandler.SearchUsersForInvite)
		// Invite member directly (admin only)
		orgs.POST("/:id/invite", g.Admin(), orgHandler.InviteMember)
		// List members — Viewer+
		orgs.GET("/:id/members", g.Viewer(), orgHandler.ListMembers)
		// Update member role (path parameter is the member tenant_id) —
		// Admin+ in caller's tenant. Changing another tenant's org role
		// is the symmetric counterpart of removing them; both must be
		// gated the same way.
		orgs.PUT("/:id/members/:tenant_id", g.Admin(), orgHandler.UpdateMemberRole)
		// Remove member (path parameter is the member tenant_id).
		// Both self-removal (caller's own tenant) and admin-removal-of-other
		// take a whole tenant out of the org, so the route must be Admin+
		// in the caller's tenant — symmetric with POST /:id/leave above.
		orgs.DELETE("/:id/members/:tenant_id", g.Admin(), orgHandler.RemoveMember)
		// List join requests (admin only) — caller's tenant must be at
		// least Admin to even see the queue (a tenant Viewer has no
		// authority to act on it).
		orgs.GET("/:id/join-requests", g.Admin(), orgHandler.ListJoinRequests)
		// Review join request (admin only)
		orgs.PUT("/:id/join-requests/:request_id/review", g.Admin(), orgHandler.ReviewJoinRequest)
		// List knowledge bases shared to this organization — Viewer+
		orgs.GET("/:id/shares", g.Viewer(), orgHandler.ListOrgShares)
		// List agents shared to this organization — Viewer+
		orgs.GET("/:id/agent-shares", g.Viewer(), orgHandler.ListOrgAgentShares)
		// List all knowledge bases in this organization (including mine) for list-page space view — Viewer+
		orgs.GET("/:id/shared-knowledge-bases", g.Viewer(), orgHandler.ListOrganizationSharedKnowledgeBases)
		// List all agents in this organization (including mine) for list-page space view — Viewer+
		orgs.GET("/:id/shared-agents", g.Viewer(), orgHandler.ListOrganizationSharedAgents)
	}

	// Knowledge base sharing routes (add to existing kb routes).
	// 分享 KB 到组织 = 让组织里所有人能读这个 KB；这跟"修改 KB 元信息"
	// 同等敏感，所以挂同款 OwnedKBOrAdmin 矩阵。Viewer 在自己空间里
	// 也不能私自把 KB 暴露出去。
	// 分享管理不通过 capability 授予（manage_spaces 也不含）；仅 full-access
	// key（空间级全权）可管理分享，scoped key 保持 default-deny。
	kbShares := g.apiKeyGroup(r.Group("/knowledge-bases/:id/shares"), apiKeyFullAccess())
	{
		// Share knowledge base
		kbShares.POST("", g.OwnedKBOrAdmin(), orgHandler.ShareKnowledgeBase)
		// List shares — Viewer+ 即可，纯读取
		kbShares.GET("", g.Viewer(), orgHandler.ListKBShares)
		// Update share permission
		kbShares.PUT("/:share_id", g.OwnedKBOrAdmin(), orgHandler.UpdateSharePermission)
		// Remove share
		kbShares.DELETE("/:share_id", g.OwnedKBOrAdmin(), orgHandler.RemoveShare)
	}

	// Agent sharing routes — same rationale as KB shares: 分享/取消分享
	// 跟修改 agent 同等敏感，挂 OwnedAgentOrAdmin。
	//
	// GET 走 OwnedAgentOrAdmin 作为 JWT 侧的 owner 校验；service 层
	// ListSharesByAgent 现在也强制 tenant 归属（与 ListSharesByKnowledgeBase
	// 对齐），这样 full-access API key（会短路路由 guard）也无法跨空间
	// 枚举他人 agent 的分享。
	// 同 KB 分享：分享管理不通过 capability 授予；仅 full-access key
	// （空间级全权）可管理 agent 分享，scoped key 保持 default-deny。
	agentShares := g.apiKeyGroup(r.Group("/agents/:id/shares"), apiKeyFullAccess())
	{
		agentShares.POST("", g.OwnedAgentOrAdmin(), orgHandler.ShareAgent)
		agentShares.GET("", g.OwnedAgentOrAdmin(), orgHandler.ListAgentShares)
		agentShares.DELETE("/:share_id", g.OwnedAgentOrAdmin(), orgHandler.RemoveAgentShare)
	}

	// Shared knowledge bases route — Viewer+
	g.apiKeyRoute(r, http.MethodGet, "/shared-knowledge-bases", apiKeyManageSpaces(apiKeyFullAccess()), g.Viewer(), orgHandler.ListSharedKnowledgeBases)
	// Shared agents route — Viewer+
	g.apiKeyRoute(r, http.MethodGet, "/shared-agents", apiKeyManageSpaces(apiKeyFullAccess()), g.Viewer(), orgHandler.ListSharedAgents)
	// "Disable by me" 是空间级偏好（写到 tenant_disabled_shared_agents），
	// 影响整个空间在会话下拉里看到的 agent 列表。任何 Viewer 改这个表就
	// 等于替整个空间做决定 — 必须 Admin+ 才允许调整。
	g.apiKeyRoute(r, http.MethodPost, "/shared-agents/disabled", apiKeyManageSpaces(apiKeyFullAccess()), g.Admin(), orgHandler.SetSharedAgentDisabledByMe)
}
