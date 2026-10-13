package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/gin-gonic/gin"
)

// fakeUserService implements only ValidateToken; the embedded interface
// satisfies the rest of the surface the middleware never calls.
type fakeUserService struct {
	interfaces.UserService
	users map[string]*types.User
	err   error
}

func (f *fakeUserService) ValidateToken(_ context.Context, token string) (*types.User, uint64, error) {
	if f.err != nil {
		return nil, 0, f.err
	}
	user, ok := f.users[token]
	if !ok {
		return nil, 0, errors.New("invalid token")
	}
	return user, user.TenantID, nil
}

func newMCPUserEngine(
	ep *types.MCPEndpoint,
	users map[string]*types.User,
	members []*types.TenantMember,
	capture *map[string]any,
) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.ContextWithFallback = true
	svc := &fakeMCPEndpointService{ep: ep, token: "mcp_secret"}
	tenantSvc := &fakeTenantService{tenant: &types.Tenant{ID: ep.TenantID, Status: "active"}}
	memberSvc := newFakeMemberService()
	for _, m := range members {
		memberSvc.members[memberKey(m.UserID, m.TenantID)] = m
	}
	deps := MCPIdentityDeps{
		TenantService: tenantSvc,
		UserService:   &fakeUserService{users: users},
		MemberService: memberSvc,
		// RBAC enforced: a missing membership must be refused rather than
		// promoted, which is the behaviour the plan requires.
		Config: mcpRBACConfig(),
	}
	r.POST("/mcp/:endpoint_id", MCPEndpointAuth(svc, tenantSvc, deps), func(c *gin.Context) {
		ctx := c.Request.Context()
		userID, _ := types.UserIDFromContext(ctx)
		role := types.TenantRoleFromContext(ctx)
		tenantID, _ := types.TenantIDFromContext(ctx)
		principal, _ := types.PrincipalFromContext(ctx)
		scope, _ := types.TenantAPIKeyScopeFromContext(ctx)
		*capture = map[string]any{
			"user_id":   userID,
			"role":      role,
			"tenant":    tenantID,
			"principal": principal,
			"scope":     scope,
		}
		c.Status(http.StatusNoContent)
	})
	return r
}

func mcpRBACConfig() *config.Config {
	enabled := true
	return &config.Config{Tenant: &config.TenantConfig{EnableRBAC: &enabled}}
}

func mcpUserEngineRequest(r *gin.Engine, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/mcp/ep-1", nil)
	req.Header.Set("Authorization", "Bearer mcp_secret")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func userEndpoint(requireUser bool, kbIDs ...string) *types.MCPEndpoint {
	return &types.MCPEndpoint{
		ID:                  "ep-1",
		TenantID:            42,
		Enabled:             true,
		RequireUserIdentity: requireUser,
		KnowledgeBaseIDs:    types.StringArray(kbIDs),
		Tools:               types.StringArray{types.MCPEndpointToolSearchKnowledge},
	}
}

// A user-scoped endpoint accepts a member and authorizes the request as that
// real user, never as the endpoint's machine identity.
func TestMCPUserIdentityGrantsMemberAccess(t *testing.T) {
	user := &types.User{ID: "u-1", Username: "alice", TenantID: 42, IsActive: true}
	members := []*types.TenantMember{{
		UserID: "u-1", TenantID: 42, Role: types.TenantRoleViewer,
		Status: types.TenantMemberStatusActive,
	}}
	var captured map[string]any
	r := newMCPUserEngine(userEndpoint(true, "kb-9"), map[string]*types.User{"jwt-1": user}, members, &captured)

	w := mcpUserEngineRequest(r, map[string]string{MCPUserTokenHeader: "jwt-1"})
	if w.Code != http.StatusNoContent {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	if uid, _ := captured["user_id"].(string); uid != "u-1" {
		t.Fatalf("the real user must be the principal, got %q", uid)
	}
	if role := captured["role"].(types.TenantRole); role != types.TenantRoleViewer {
		t.Fatalf("role = %q, want viewer", role)
	}
	if tenant := captured["tenant"].(uint64); tenant != 42 {
		t.Fatalf("tenant = %d, want 42", tenant)
	}
	principal := captured["principal"].(types.Principal)
	if principal.Type != types.PrincipalWebUser || principal.ID != "u-1" {
		t.Fatalf("principal = %+v, want the web user", principal)
	}
	// The endpoint's scope still bounds the call: kb-9 in, kb-1 out.
	scope := captured["scope"].(types.TenantAPIKeyScope)
	if !scope.AllowsKnowledgeBase("kb-9") || scope.AllowsKnowledgeBase("kb-1") {
		t.Fatalf("endpoint scope must still apply: %+v", scope)
	}
}

// A user-scoped endpoint without the user token must not fall back to the
// machine identity: that would silently bypass the whole feature.
func TestMCPUserIdentityRequiredButMissing(t *testing.T) {
	var captured map[string]any
	r := newMCPUserEngine(userEndpoint(true), map[string]*types.User{}, nil, &captured)

	w := mcpUserEngineRequest(r, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", w.Code)
	}
	if captured != nil {
		t.Fatal("the handler must not run without a user identity")
	}
}

func TestMCPUserIdentityRejectsInvalidToken(t *testing.T) {
	var captured map[string]any
	r := newMCPUserEngine(userEndpoint(true), map[string]*types.User{}, nil, &captured)

	w := mcpUserEngineRequest(r, map[string]string{MCPUserTokenHeader: "expired-token"})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", w.Code)
	}
	if captured != nil {
		t.Fatal("an invalid token must not produce a session")
	}
}

// A presented-but-unusable token on a machine endpoint is still rejected: the
// request is never downgraded to the machine identity after a failed check.
func TestMCPUserIdentityNeverDowngradesToMachine(t *testing.T) {
	var captured map[string]any
	r := newMCPUserEngine(userEndpoint(false), map[string]*types.User{}, nil, &captured)

	w := mcpUserEngineRequest(r, map[string]string{MCPUserTokenHeader: "bad"})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401 (no downgrade)", w.Code)
	}
	if captured != nil {
		t.Fatal("no session may be attached after a failed user check")
	}
}

func TestMCPUserIdentityRejectsInactiveUser(t *testing.T) {
	user := &types.User{ID: "u-2", TenantID: 42, IsActive: false}
	var captured map[string]any
	r := newMCPUserEngine(userEndpoint(true), map[string]*types.User{"jwt-2": user}, nil, &captured)

	w := mcpUserEngineRequest(r, map[string]string{MCPUserTokenHeader: "jwt-2"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403", w.Code)
	}
	if captured != nil {
		t.Fatal("an inactive user must not reach the handler")
	}
}

// Revoking membership must revoke access even though the JWT is still valid.
func TestMCPUserIdentityRejectsRevokedMembership(t *testing.T) {
	user := &types.User{ID: "u-3", TenantID: 7, IsActive: true} // home tenant differs
	members := []*types.TenantMember{{
		UserID: "u-3", TenantID: 42, Role: types.TenantRoleContributor,
		Status: types.TenantMemberStatusSuspended,
	}}
	var captured map[string]any
	r := newMCPUserEngine(userEndpoint(true), map[string]*types.User{"jwt-3": user}, members, &captured)

	w := mcpUserEngineRequest(r, map[string]string{MCPUserTokenHeader: "jwt-3"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403", w.Code)
	}
	if captured != nil {
		t.Fatal("a revoked member must not reach the handler")
	}
}

// X-Tenant-ID must not be usable to pivot an endpoint into another workspace.
func TestMCPUserIdentityRejectsForeignWorkspaceHeader(t *testing.T) {
	user := &types.User{ID: "u-4", TenantID: 42, IsActive: true}
	var captured map[string]any
	r := newMCPUserEngine(userEndpoint(true), map[string]*types.User{"jwt-4": user}, nil, &captured)

	w := mcpUserEngineRequest(r, map[string]string{
		MCPUserTokenHeader: "jwt-4",
		"X-Tenant-ID":      "99",
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403", w.Code)
	}
	if captured != nil {
		t.Fatal("a mismatched workspace header must not reach the handler")
	}
}

func TestMCPUserIdentityAcceptsMatchingWorkspaceHeader(t *testing.T) {
	user := &types.User{ID: "u-5", TenantID: 42, IsActive: true}
	members := []*types.TenantMember{{
		UserID: "u-5", TenantID: 42, Role: types.TenantRoleOwner,
		Status: types.TenantMemberStatusActive,
	}}
	var captured map[string]any
	r := newMCPUserEngine(userEndpoint(true), map[string]*types.User{"jwt-5": user}, members, &captured)

	w := mcpUserEngineRequest(r, map[string]string{
		MCPUserTokenHeader: "jwt-5",
		"X-Tenant-ID":      "42",
	})
	if w.Code != http.StatusNoContent {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
}

// Existing machine-identity clients must keep working unchanged.
func TestMCPMachineIdentityStillWorks(t *testing.T) {
	var captured map[string]any
	r := newMCPUserEngine(userEndpoint(false), nil, nil, &captured)

	w := mcpUserEngineRequest(r, nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	if uid, _ := captured["user_id"].(string); uid != "mcp-ep-1" {
		t.Fatalf("machine user = %q, want the synthetic endpoint user", uid)
	}
	principal := captured["principal"].(types.Principal)
	if principal.Type != types.PrincipalMCPEndpoint {
		t.Fatalf("principal = %+v, want the endpoint principal", principal)
	}
	if role := captured["role"].(types.TenantRole); role != types.TenantRoleViewer {
		t.Fatalf("machine role = %q, want viewer", role)
	}
}

// A machine endpoint ignores an unknown user token rather than failing, which
// is what keeps old clients that happen to send the header working.
func TestMCPMachineIdentityIgnoresUnknownUserHeaderWhenServicesMissing(t *testing.T) {
	captured := map[string]any{}
	// Simulate a deployment wired without the identity services.
	gin.SetMode(gin.TestMode)
	r2 := gin.New()
	r2.ContextWithFallback = true
	svc := &fakeMCPEndpointService{ep: userEndpoint(false), token: "mcp_secret"}
	r2.POST("/mcp/:endpoint_id", MCPEndpointAuth(svc, &fakeTenantService{tenant: &types.Tenant{ID: 42, Status: "active"}}),
		func(c *gin.Context) {
			uid, _ := types.UserIDFromContext(c.Request.Context())
			captured["user_id"] = uid
			c.Status(http.StatusNoContent)
		})

	req := httptest.NewRequest(http.MethodPost, "/mcp/ep-1", nil)
	req.Header.Set("Authorization", "Bearer mcp_secret")
	w := httptest.NewRecorder()
	r2.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status %d, want the machine path to work", w.Code)
	}
	if uid, _ := captured["user_id"].(string); uid != "mcp-ep-1" {
		t.Fatal("the machine identity must still be attached")
	}
}

// An endpoint that requires user identity but is wired without the identity
// services must fail closed instead of silently degrading.
func TestMCPUserIdentityFailsClosedWhenNotWired(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.ContextWithFallback = true
	svc := &fakeMCPEndpointService{ep: userEndpoint(true), token: "mcp_secret"}
	r.POST("/mcp/:endpoint_id",
		MCPEndpointAuth(svc, &fakeTenantService{tenant: &types.Tenant{ID: 42, Status: "active"}}),
		func(c *gin.Context) { c.Status(http.StatusNoContent) })

	req := httptest.NewRequest(http.MethodPost, "/mcp/ep-1", nil)
	req.Header.Set("Authorization", "Bearer mcp_secret")
	req.Header.Set(MCPUserTokenHeader, "anything")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	// 503 AUTH_DEPENDENCY_UNAVAILABLE: the deployment is at fault, and the
	// request must not be authorized on a default role.
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503 AUTH_DEPENDENCY_UNAVAILABLE (fail closed)", w.Code)
	}
	if !strings.Contains(w.Body.String(), CodeAuthDependencyUnavailable) {
		t.Fatalf("body %s should name %s", w.Body.String(), CodeAuthDependencyUnavailable)
	}
}

// A user-scoped endpoint wired with a user service but WITHOUT the membership
// service must also fail closed: skipping the membership check would mean
// authorizing on a default role, which the plan forbids.
func TestMCPUserIdentityFailsClosedWithoutMemberService(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.ContextWithFallback = true
	svc := &fakeMCPEndpointService{ep: userEndpoint(true), token: "mcp_secret"}
	deps := MCPIdentityDeps{
		TenantService: &fakeTenantService{tenant: &types.Tenant{ID: 42, Status: "active"}},
		UserService:   &fakeUserService{users: map[string]*types.User{"jwt": {ID: "u-1", TenantID: 42, IsActive: true}}},
		// MemberService deliberately nil.
	}
	r.POST("/mcp/:endpoint_id",
		MCPEndpointAuth(svc, deps.TenantService, deps),
		func(c *gin.Context) { c.Status(http.StatusNoContent) })

	req := httptest.NewRequest(http.MethodPost, "/mcp/ep-1", nil)
	req.Header.Set("Authorization", "Bearer mcp_secret")
	req.Header.Set(MCPUserTokenHeader, "jwt")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503 (membership service missing)", w.Code)
	}
}
