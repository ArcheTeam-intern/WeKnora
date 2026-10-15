package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/Tencent/WeKnora/internal/infrastructure/docparser"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/mark3labs/mcp-go/mcp"
)

// listParserEnginesTool exposes the deployment's document parser engines so a
// client can CHOOSE a parser before importing, instead of guessing engine
// names. Safe fields only: no vendor endpoints, credentials or config keys.
func listParserEnginesTool() mcp.Tool {
	return mcp.NewTool(types.MCPEndpointToolListParserEngines,
		mcp.WithDescription("List the document parser engines this deployment can use for uploads "+
			"(name, supported file types, availability and the reason when unavailable). "+
			"Use it before importing to pick a parser engine for process_config in import_document_file, "+
			"instead of guessing engine names."),
		mcp.WithReadOnlyHintAnnotation(true),
	)
}

// tenantParserEngineOverrides extracts the workspace's parser configuration
// exactly like the REST engine listing does, so both surfaces answer
// identically for the same workspace.
func tenantParserEngineOverrides(ctx context.Context) map[string]string {
	tenant, _ := types.TenantInfoFromContext(ctx)
	if tenant == nil {
		return nil
	}
	var overrides map[string]string
	if tenant.ParserEngineConfig != nil {
		overrides = tenant.ParserEngineConfig.ToOverridesMap()
	}
	if creds := tenant.Credentials.GetWeKnoraCloud(); creds != nil {
		if overrides == nil {
			overrides = make(map[string]string)
		}
		overrides["weknoracloud_app_id"] = creds.AppID
	}
	return overrides
}

// listParserEngines resolves the workspace's engine catalog once, shared by
// the list tool and by import process_config validation so both surfaces agree
// on what is available.
func (s *Server) listParserEngines(ctx context.Context) ([]types.ParserEngineInfo, bool) {
	overrides := tenantParserEngineOverrides(ctx)
	connected := s.documentReader != nil && s.documentReader.IsConnected()
	var remoteEngines []types.ParserEngineInfo
	if connected {
		// 与 REST 引擎列表一致：远端发现失败降级为仅本地引擎，不致失败。
		if engines, err := s.documentReader.ListEngines(ctx, overrides); err == nil {
			remoteEngines = engines
		}
	}
	return docparser.ListAllEngines(connected, overrides, remoteEngines), connected
}

func (s *Server) handleListParserEngines(
	ctx context.Context, _ mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	engines, connected := s.listParserEngines(ctx)

	out := make([]map[string]any, 0, len(engines))
	for _, e := range engines {
		out = append(out, map[string]any{
			"name":               e.Name,
			"description":        e.Description,
			"file_types":         e.FileTypes,
			"available":          e.Available,
			"unavailable_reason": e.UnavailableReason,
		})
	}
	var names []string
	for _, e := range engines {
		marker := ""
		if !e.Available {
			marker = "（不可用）"
		}
		names = append(names, e.Name+marker)
	}
	return mcp.NewToolResultStructured(map[string]any{
		"engines":             out,
		"count":               len(out),
		"docreader_connected": connected,
		"display_type":        "parser_engines",
	}, fmt.Sprintf("解析引擎 %d 个：%s。process_config 里引用引擎名（不带不可用标记）。", len(out), strings.Join(names, "、"))), nil
}
