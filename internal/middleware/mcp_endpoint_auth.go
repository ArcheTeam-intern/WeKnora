package middleware

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/Tencent/WeKnora/internal/application/service"
	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/mcpscope"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/gin-gonic/gin"
)

// MCPUserTokenHeader carries a first-party WeKnora user JWT on the MCP server
// surface. It is deliberately separate from Authorization, which stays the
// endpoint credential, so an endpoint can require a real user without losing
// its own machine identity for the token lookup.
const MCPUserTokenHeader = "X-WeKnora-User-Token"

// CodeAuthDependencyUnavailable is returned when a user-scoped endpoint is used
// on a deployment that did not wire the identity services it needs. It is a
// deployment fault, so it maps to 503 and is never softened into a Viewer
// fallback.
const CodeAuthDependencyUnavailable = "AUTH_DEPENDENCY_UNAVAILABLE"

// MCPIdentityDeps are the services a user-scoped MCP endpoint needs in order
// to validate the presented user JWT and re-authorize the caller inside the
// endpoint's workspace. They are optional: an endpoint that does not require a
// user identity keeps working with the machine identity alone.
type MCPIdentityDeps struct {
	TenantService  interfaces.TenantService
	UserService    interfaces.UserService
	MemberService  interfaces.TenantMemberService
	Config         *config.Config
	TenantRequired bool
}

// MCPEndpointAuth authenticates calls on the workspace MCP server surface
// (/mcp/:endpoint_id). The client presents the endpoint's bearer token; on
// success the request carries the endpoint's workspace and an API-key style
// scope derived from the endpoint's tool allowlist and knowledge-base scope,
// so every downstream service applies the same checks it applies to a scoped
// API key.
//
// Two identity modes exist and are decided by the endpoint itself:
//
//   - Machine mode (RequireUserIdentity=false, the historical default): the
//     endpoint's token is the only credential and the request runs as a
//     synthetic machine user. Existing MCP clients are unaffected.
//   - User mode (RequireUserIdentity=true): the caller must additionally send
//     a valid WeKnora user JWT in X-WeKnora-User-Token. The request then runs
//     as that real user, and the endpoint's scope only narrows what the user
//     may already do — it never widens it. A presented but invalid JWT is
//     rejected outright; the request is never downgraded back to the machine
//     identity.
//
// The services needed for the user mode are optional variadic arguments so
// callers that only run endpoints in machine mode (and older tests) keep
// working unchanged.
func MCPEndpointAuth(
	svc interfaces.MCPEndpointService,
	tenantSvc interfaces.TenantService,
	deps ...MCPIdentityDeps,
) gin.HandlerFunc {
	var identity MCPIdentityDeps
	if len(deps) > 0 {
		identity = deps[0]
	}
	if identity.TenantService == nil {
		identity.TenantService = tenantSvc
	}
	return func(c *gin.Context) {
		endpointID := strings.TrimSpace(c.Param("endpoint_id"))
		token := extractMCPEndpointToken(c)
		if endpointID == "" || token == "" {
			c.Header("WWW-Authenticate", `Bearer realm="weknora-mcp"`)
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized: missing bearer token"})
			c.Abort()
			return
		}
		ep, err := svc.Authenticate(c.Request.Context(), endpointID, token)
		if err != nil {
			switch {
			case errors.Is(err, service.ErrMCPEndpointDisabled):
				c.JSON(http.StatusForbidden, gin.H{"error": "mcp endpoint disabled"})
			case errors.Is(err, service.ErrMCPEndpointTokenInvalid), errors.Is(err, service.ErrMCPEndpointNotFound):
				c.Header("WWW-Authenticate", `Bearer realm="weknora-mcp"`)
				c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized: invalid token"})
			default:
				logger.Warnf(c.Request.Context(), "[mcp-endpoint] auth lookup failed: %v", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "authentication unavailable"})
			}
			c.Abort()
			return
		}
		tenant, err := tenantSvc.GetTenantByID(c.Request.Context(), ep.TenantID)
		if err != nil || tenant == nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "workspace unavailable"})
			c.Abort()
			return
		}
		if strings.EqualFold(tenant.Status, "inactive") || strings.EqualFold(tenant.Status, "disabled") {
			c.JSON(http.StatusForbidden, gin.H{"error": "workspace inactive"})
			c.Abort()
			return
		}

		// The optional knowledge-base selection is parsed BEFORE any identity
		// work so a malformed header fails as a clean 400 rather than after the
		// user has been authenticated.
		requestScope, scopeErr := parseRequestScopeHeader(c)
		if scopeErr != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": scopeErr.Message, "code": scopeErr.Code})
			c.Abort()
			return
		}
		if requestScope.Present {
			c.Request = c.Request.WithContext(
				mcpscope.WithScope(c.Request.Context(), requestScope))
		}

		scope := types.MCPEndpointScope(ep)

		if userToken := strings.TrimSpace(c.GetHeader(MCPUserTokenHeader)); userToken != "" {
			// A presented user credential is always authoritative: it either
			// authenticates as a real user (and the endpoint's scope narrows
			// that user's rights) or the request is rejected.
			if !identity.machineOnly() {
				if !authenticateMCPUser(c, ep, tenant, scope, identity, userToken) {
					return
				}
				c.Next()
				return
			}
			if ep.RequireUserIdentity {
				// Configured for user identity but the deployment did not wire
				// the services it needs. The plan is explicit: refuse with 503
				// AUTH_DEPENDENCY_UNAVAILABLE rather than fall back to a default
				// Viewer or skip the membership check, because either would turn
				// a misconfiguration into an authorization bypass.
				c.JSON(http.StatusServiceUnavailable, gin.H{
					"error": "user identity is not configured on this server",
					"code":  CodeAuthDependencyUnavailable,
				})
				c.Abort()
				return
			}
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized: invalid or expired user token"})
			c.Abort()
			return
		}

		if ep.RequireUserIdentity {
			c.Header("WWW-Authenticate", `Bearer realm="weknora-mcp"`)
			c.JSON(http.StatusUnauthorized, gin.H{
				"error": "unauthorized: this endpoint requires " + MCPUserTokenHeader,
				"code":  "UNAUTHORIZED",
			})
			c.Abort()
			return
		}

		user := &types.User{
			ID:       fmt.Sprintf("mcp-%s", ep.ID),
			Username: fmt.Sprintf("mcp-%s", ep.ID),
			Email:    fmt.Sprintf("mcp-%s@mcp.local", ep.ID),
			TenantID: ep.TenantID,
			IsActive: true,
		}
		applyAuthSession(c, authSession{
			User:        user,
			Principal:   types.MCPEndpointPrincipal(ep.TenantID, ep.ID),
			TenantID:    ep.TenantID,
			Tenant:      tenant,
			Role:        types.TenantRoleViewer,
			APIKeyScope: &scope,
			Extra:       map[types.ContextKey]any{types.MCPEndpointContextKey: ep},
		})
		c.Next()
	}
}

// machineOnly reports that the middleware was wired without the services the
// user-identity branch needs.
//
// Both the user service AND the membership service are required. Skipping the
// membership check is equivalent to skipping the role resolution, which the plan
// forbids: a user-scoped endpoint whose deployment lacks either service must fail
// closed with 503 AUTH_DEPENDENCY_UNAVAILABLE instead of authorizing on a
// default role.
func (d MCPIdentityDeps) machineOnly() bool {
	return d.UserService == nil || d.MemberService == nil
}

// authenticateMCPUser validates the presented user JWT and re-authorizes the
// caller inside the endpoint's workspace. It writes the error response and
// returns false when the caller may not proceed.
//
// The user is authorized before the request context switches to the user's
// identity, so no partially authenticated session is ever observable by a
// downstream handler.
func authenticateMCPUser(
	c *gin.Context,
	ep *types.MCPEndpoint,
	tenant *types.Tenant,
	scope types.TenantAPIKeyScope,
	identity MCPIdentityDeps,
	userToken string,
) bool {
	ctx := c.Request.Context()
	user, _, err := identity.UserService.ValidateToken(ctx, userToken)
	if err != nil || user == nil {
		logger.Warnf(ctx, "[mcp-endpoint] endpoint %s rejected user token: %v", ep.ID, err)
		c.Header("WWW-Authenticate", `Bearer realm="weknora-mcp"`)
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized: invalid or expired user token"})
		c.Abort()
		return false
	}
	if !user.IsActive {
		c.JSON(http.StatusForbidden, gin.H{"error": "forbidden: user is inactive"})
		c.Abort()
		return false
	}
	// The user must reach the endpoint's workspace, either as a member or as a
	// cross-workspace superuser. This is the same rule the REST surface uses,
	// so an MCP call can never address a workspace the user cannot open.
	if !IsTenantAccessible(ctx, user, ep.TenantID, identity.MemberService, identity.Config) {
		logger.Warnf(ctx, "[mcp-endpoint] user %s has no access to workspace %d (endpoint %s)",
			user.ID, ep.TenantID, ep.ID)
		c.JSON(http.StatusForbidden, gin.H{"error": "forbidden: not a member of this workspace",
			"code": "FORBIDDEN"})
		c.Abort()
		return false
	}

	// X-Tenant-ID, when present, must name the endpoint's workspace: an
	// endpoint is bound to exactly one workspace and may not be used as a
	// pivot into another one.
	if header := strings.TrimSpace(c.GetHeader("X-Tenant-ID")); header != "" {
		targetTenantID, parseErr := strconv.ParseUint(header, 10, 64)
		if parseErr != nil || targetTenantID == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid X-Tenant-ID header"})
			c.Abort()
			return false
		}
		if targetTenantID != ep.TenantID {
			logger.Warnf(ctx, "[mcp-endpoint] user %s requested workspace %d on endpoint %s bound to %d",
				user.ID, targetTenantID, ep.ID, ep.TenantID)
			c.JSON(http.StatusForbidden, gin.H{"error": "forbidden: workspace does not match this endpoint"})
			c.Abort()
			return false
		}
	}

	// The role is resolved from the caller's real membership. There is no
	// Viewer default here: the plan forbids skipping the membership check, and
	// machineOnly() has already guaranteed the service exists.
	resolvedRole, ok := resolveTenantRole(ctx, identity.MemberService, user, ep.TenantID,
		ep.TenantID != user.TenantID, identity.Config)
	if !ok {
		logger.Warnf(ctx, "[mcp-endpoint] user %s has no active membership in workspace %d",
			user.ID, ep.TenantID)
		c.JSON(http.StatusForbidden, gin.H{"error": "forbidden: not a member of this workspace",
			"code": "FORBIDDEN"})
		c.Abort()
		return false
	}
	role := resolvedRole

	applyAuthSession(c, authSession{
		User:        user,
		Principal:   types.Principal{Type: types.PrincipalWebUser, ID: user.ID},
		TenantID:    ep.TenantID,
		Tenant:      tenant,
		Role:        role,
		SystemAdmin: user.IsSystemAdmin,
		APIKeyScope: &scope,
		Extra:       map[types.ContextKey]any{types.MCPEndpointContextKey: ep},
	})
	return true
}

// extractMCPEndpointToken reads "Authorization: Bearer mcp_..." only. Query
// string tokens are rejected so they never land in access logs.
func extractMCPEndpointToken(c *gin.Context) string {
	auth := strings.TrimSpace(c.GetHeader("Authorization"))
	if len(auth) > 7 && strings.EqualFold(auth[:7], "Bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	return ""
}

// parseRequestScopeHeader reads the optional knowledge-base selection header.
//
// Absent header -> zero scope, meaning "not narrowed" (legacy clients).
// Present but empty, non-UUID, or more than the cap -> a 400-worthy error, which
// is what stops an empty selection from silently meaning "all knowledge bases".
func parseRequestScopeHeader(c *gin.Context) (mcpscope.Scope, *mcpscope.Error) {
	value, present := c.Request.Header[http.CanonicalHeaderKey(mcpscope.KnowledgeBaseScopeHeader)]
	if !present {
		return mcpscope.Scope{}, nil
	}
	raw := ""
	if len(value) > 0 {
		raw = value[0]
	}
	scope, err := mcpscope.Parse(raw)
	if err != nil {
		return mcpscope.Scope{}, mcpscope.AsError(err)
	}
	return scope, nil
}

// MCPEndpointFromContext returns the endpoint authenticated for this request.
func MCPEndpointFromContext(ctx context.Context) (*types.MCPEndpoint, bool) {
	ep, ok := ctx.Value(types.MCPEndpointContextKey).(*types.MCPEndpoint)
	return ep, ok && ep != nil
}
