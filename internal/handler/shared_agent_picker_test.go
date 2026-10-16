package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/handler/dto"
	"github.com/Tencent/WeKnora/internal/middleware"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// A shared agent invokes MCP services that live in ITS OWNER's workspace. The @
// picker used to read the caller's own workspace, so it offered the caller's
// own services under "all", which the backend then dropped from the mention.
const lenderWorkspace = uint64(84)

// testSkillTenantID was the caller workspace of the removed skill picker; the
// MCP picker tests authenticate as the same borrower.
const testSkillTenantID = uint64(7)

func pickerAuth(c *gin.Context) {
	c.Set(types.TenantIDContextKey.String(), testSkillTenantID)
	c.Set(types.UserIDContextKey.String(), "user-1")
	c.Next()
}

type pickerMCPStub struct {
	interfaces.MCPServiceService
	listedTenant  uint64
	listedIDs     []string
	ownListCalls  int
	services      []*types.MCPService
	summaryTenant uint64
}

func (s *pickerMCPStub) ListMCPServices(
	_ context.Context, tenantID uint64,
) ([]*types.MCPService, error) {
	s.ownListCalls++
	s.listedTenant = tenantID
	return s.services, nil
}

func (s *pickerMCPStub) ListMCPServicesByIDs(
	_ context.Context, tenantID uint64, ids []string,
) ([]*types.MCPService, error) {
	s.listedTenant, s.listedIDs = tenantID, ids
	out := make([]*types.MCPService, 0, len(ids))
	for _, id := range ids {
		for _, svc := range s.services {
			if svc.ID == id {
				out = append(out, svc)
			}
		}
	}
	return out, nil
}

func (s *pickerMCPStub) ListMCPMetadataSummaries(
	_ context.Context, tenantID uint64, _ []*types.MCPService,
) (map[string]*types.MCPMetadataSummary, error) {
	s.summaryTenant = tenantID
	return nil, nil
}

func newPickerMCPRouter(h *MCPServiceHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.ErrorHandler())
	r.Use(pickerAuth)
	r.GET("/mcp-services", h.ListMCPServices)
	return r
}

func sharedMCPAgent(mode string, ids ...string) *types.CustomAgent {
	return &types.CustomAgent{
		ID: "agent-1", TenantID: lenderWorkspace,
		Config: types.CustomAgentConfig{MCPSelectionMode: mode, MCPServices: ids},
	}
}

func lenderService(id, name string) *types.MCPService {
	url := "https://internal.lender.example/mcp"
	return &types.MCPService{
		ID: id, TenantID: lenderWorkspace, Name: name, Enabled: true,
		URL: &url, Headers: types.MCPHeaders{"X-Secret": "nope"},
		AuthConfig: &types.MCPAuthConfig{APIKey: "sk-lender"},
	}
}

func getMCP(t *testing.T, r *gin.Engine, query string) (int, []*dto.MCPServiceResponse, []byte) {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/mcp-services"+query, nil))
	var body struct {
		Data []*dto.MCPServiceResponse `json:"data"`
	}
	if w.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	}
	return w.Code, body.Data, w.Body.Bytes()
}

func TestListMCPServicesServesASharedAgentsPresetFromItsOwnWorkspace(t *testing.T) {
	mcp := &pickerMCPStub{services: []*types.MCPService{
		lenderService("svc-a", "Lender A"),
		lenderService("svc-b", "Lender B"),
	}}
	agents := &scopedAgentStub{agent: sharedMCPAgent("selected", "svc-b")}
	router := newPickerMCPRouter(NewMCPServiceHandler(mcp, nil, nil, nil, agents))

	code, data, raw := getMCP(t, router, "?agent_id=agent-1&agent_source_tenant_id=84")

	require.Equal(t, http.StatusOK, code)
	require.Len(t, data, 1)
	require.Equal(t, "svc-b", data[0].ID)
	require.Equal(t, "Lender B", data[0].Name)
	require.Equal(t, lenderWorkspace, mcp.listedTenant)
	require.Equal(t, []string{"svc-b"}, mcp.listedIDs)
	require.Zero(t, mcp.ownListCalls, "the caller's own services are irrelevant here")
	require.Equal(t, lenderWorkspace, mcp.summaryTenant)

	// The borrower configures none of this and must not see how the owner
	// wired the service up.
	require.Nil(t, data[0].URL)
	require.Nil(t, data[0].Headers)
	require.Nil(t, data[0].AuthConfig)
	require.Nil(t, data[0].Credentials)
	require.NotContains(t, string(raw), "internal.lender.example")
	require.NotContains(t, string(raw), "sk-lender")
}

// resolvePerRequestMCPScope intersects a shared agent's @mention against its
// explicit preset, which is empty under "all" and "none". Offering the owner's
// whole inventory there would put services in the picker that every mention
// then silently drops.
func TestListMCPServicesOffersNothingOutsideASharedAgentsPreset(t *testing.T) {
	for _, mode := range []string{"all", "none", ""} {
		mcp := &pickerMCPStub{services: []*types.MCPService{lenderService("svc-a", "Lender A")}}
		agents := &scopedAgentStub{agent: sharedMCPAgent(mode, "svc-a")}
		router := newPickerMCPRouter(NewMCPServiceHandler(mcp, nil, nil, nil, agents))

		code, data, _ := getMCP(t, router, "?agent_id=agent-1&agent_source_tenant_id=84")

		require.Equal(t, http.StatusOK, code, mode)
		require.Empty(t, data, "mode %q pins nothing, so it offers nothing", mode)
		require.Zero(t, mcp.ownListCalls, mode)
	}
}

func TestListMCPServicesSkipsADisabledSharedService(t *testing.T) {
	disabled := lenderService("svc-a", "Lender A")
	disabled.Enabled = false
	mcp := &pickerMCPStub{services: []*types.MCPService{disabled}}
	agents := &scopedAgentStub{agent: sharedMCPAgent("selected", "svc-a")}
	router := newPickerMCPRouter(NewMCPServiceHandler(mcp, nil, nil, nil, agents))

	code, data, _ := getMCP(t, router, "?agent_id=agent-1&agent_source_tenant_id=84")

	require.Equal(t, http.StatusOK, code)
	require.Empty(t, data, "registerMCPTools skips disabled services, so the picker must too")
}

func TestListMCPServicesKeepsTheOwnWorkspacePathUnchanged(t *testing.T) {
	own := lenderService("svc-own", "Mine")
	own.TenantID = testSkillTenantID
	mcp := &pickerMCPStub{services: []*types.MCPService{own}}
	agents := &scopedAgentStub{agent: sharedMCPAgent("selected", "svc-b")}
	router := newPickerMCPRouter(NewMCPServiceHandler(mcp, nil, nil, nil, agents))

	code, data, _ := getMCP(t, router, "?agent_id=agent-1")

	require.Equal(t, http.StatusOK, code)
	require.Len(t, data, 1)
	require.Equal(t, 1, mcp.ownListCalls)
	require.Equal(t, testSkillTenantID, mcp.listedTenant)
	require.Zero(t, agents.caller, "no share lookup for an own agent")
	// The full shape is unchanged for a workspace's own services: credential
	// metadata is what the settings UI renders its badges from.
	require.NotNil(t, data[0].Credentials)
}

// NewMCPServiceResponse reveals URL / headers / env vars to an Admin, and a
// borrower can be an Admin of their OWN workspace. The cross-workspace shape
// must therefore be narrow by construction, not by role.
func TestListMCPServicesNeverRevealsOwnerConfigToAnAdminBorrower(t *testing.T) {
	mcp := &pickerMCPStub{services: []*types.MCPService{lenderService("svc-a", "Lender A")}}
	agents := &scopedAgentStub{agent: sharedMCPAgent("selected", "svc-a")}
	h := NewMCPServiceHandler(mcp, nil, nil, nil, agents)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.ErrorHandler())
	r.Use(func(c *gin.Context) {
		pickerAuth(c)
		c.Request = c.Request.WithContext(context.WithValue(
			c.Request.Context(), types.TenantRoleContextKey, types.TenantRoleOwner))
		c.Next()
	})
	r.GET("/mcp-services", h.ListMCPServices)

	code, data, raw := getMCP(t, r, "?agent_id=agent-1&agent_source_tenant_id=84")

	require.Equal(t, http.StatusOK, code)
	require.Len(t, data, 1)
	require.Nil(t, data[0].URL)
	require.Nil(t, data[0].Headers)
	require.Nil(t, data[0].Credentials)
	require.NotContains(t, string(raw), "internal.lender.example")
	require.NotContains(t, string(raw), "sk-lender")
}

func TestListMCPServicesRefusesAnUnreachableShare(t *testing.T) {
	mcp := &pickerMCPStub{services: []*types.MCPService{lenderService("svc-a", "Lender A")}}
	agents := &scopedAgentStub{agent: nil}
	router := newPickerMCPRouter(NewMCPServiceHandler(mcp, nil, nil, nil, agents))

	code, _, _ := getMCP(t, router, "?agent_id=agent-1&agent_source_tenant_id=84")

	require.Equal(t, http.StatusForbidden, code)
	require.Zero(t, mcp.ownListCalls)
}
