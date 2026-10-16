package router

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Tencent/WeKnora/internal/handler"
	"github.com/gin-gonic/gin"
)

// The core path must be registered before the legacy parameterised route, or
// gin would read "core" as an endpoint id and the main product would reach the
// twenty-two-tool catalogue by accident.
func TestCoreMCPPathIsRegisteredBeforeLegacy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	routes := []string{
		handler.MCPEndpointPath + "core/:endpoint_id",
		handler.MCPEndpointPath + ":endpoint_id",
	}
	for _, route := range routes {
		for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodDelete} {
			engine.Handle(method, route, func(c *gin.Context) {
				c.String(http.StatusOK, c.FullPath()+"|"+c.Param("endpoint_id"))
			})
		}
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/mcp/core/ep-1", nil)
	engine.ServeHTTP(recorder, request)
	if got := recorder.Body.String(); got != "/mcp/core/:endpoint_id|ep-1" {
		t.Fatalf("core path resolved to %q", got)
	}

	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/mcp/ep-2", nil)
	engine.ServeHTTP(recorder, request)
	if got := recorder.Body.String(); got != "/mcp/:endpoint_id|ep-2" {
		t.Fatalf("legacy path resolved to %q", got)
	}
}
