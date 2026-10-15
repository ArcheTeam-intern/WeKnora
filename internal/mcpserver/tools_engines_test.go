package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/mark3labs/mcp-go/mcp"
)

// stubEngineReader fakes just the two DocumentReader methods the engine
// catalog needs, so the tool and the import validator share one engine list.
type stubEngineReader struct {
	interfaces.DocumentReader
	engines   []types.ParserEngineInfo
	connected bool
	fail      bool
}

func (s *stubEngineReader) IsConnected() bool { return s.connected }

func (s *stubEngineReader) ListEngines(context.Context, map[string]string) ([]types.ParserEngineInfo, error) {
	if s.fail {
		return nil, errors.New("docreader unavailable")
	}
	return s.engines, nil
}

func engineTestServer() *Server {
	return &Server{documentReader: &stubEngineReader{
		connected: true,
		engines: []types.ParserEngineInfo{
			// Remote-only names: a local engine of the same name keeps its
			// Go-side availability, so these exercise the discovery path.
			{Name: "docreader-pdf-pro", Description: "remote PDF engine",
				FileTypes: []string{"pdf", "docx"}, Available: true},
			{Name: "docreader-legacy", Description: "remote legacy engine",
				FileTypes: []string{"pdf"}, Available: false, UnavailableReason: "service not configured"},
		},
	}}
}

func processConfigRequest(config map[string]any) mcp.CallToolRequest {
	return mcp.CallToolRequest{Params: mcp.CallToolParams{
		Arguments: map[string]any{"process_config": config},
	}}
}

func parserRule(engine string, fileTypes ...string) map[string]any {
	values := make([]any, 0, len(fileTypes))
	for _, ft := range fileTypes {
		values = append(values, ft)
	}
	return map[string]any{"engine": engine, "file_types": values}
}

// 引擎目录只返回安全字段：名称/类型/可用性与原因；绝不返回服务地址或凭据。
func TestListParserEnginesExposesOnlySafeFields(t *testing.T) {
	srv := engineTestServer()
	result, err := srv.handleListParserEngines(context.Background(), mcp.CallToolRequest{})
	if err != nil {
		t.Fatalf("engine tool failed: %v", err)
	}
	if result.IsError {
		t.Fatalf("engine tool returned an error result: %#v", result.Content)
	}
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	payload := string(raw)
	if !strings.Contains(payload, `"docreader-pdf-pro"`) || !strings.Contains(payload, `"available":true`) {
		t.Fatalf("engine list missing the available engine: %s", payload)
	}
	if !strings.Contains(payload, "service not configured") {
		t.Fatalf("unavailable reason must be reported: %s", payload)
	}
	for _, forbidden := range []string{"addr", "password", "api_key", "secret", "transport", "://"} {
		if strings.Contains(strings.ToLower(payload), forbidden) {
			t.Fatalf("engine list leaked %q: %s", forbidden, payload)
		}
	}
}

// process_config 只接受 parser_engine_rules，且引擎必须是本工作空间真实可用的。
func TestParseImportProcessConfigValidatesEngineChoice(t *testing.T) {
	srv := engineTestServer()

	rules, err := srv.parseImportProcessConfig(context.Background(), processConfigRequest(map[string]any{
		"parser_engine_rules": []any{parserRule("docreader-pdf-pro", ".PDF", "pdf", "docx")},
	}))
	if err != nil {
		t.Fatalf("valid config must pass: %v", err)
	}
	if rules == nil || len(rules.ParserEngineRules) != 1 {
		t.Fatalf("expected one rule, got %#v", rules)
	}
	rule := rules.ParserEngineRules[0]
	if rule.Engine != "docreader-pdf-pro" {
		t.Fatalf("engine not preserved: %q", rule.Engine)
	}
	if len(rule.FileTypes) != 2 || rule.FileTypes[0] != "pdf" || rule.FileTypes[1] != "docx" {
		t.Fatalf("file types must be normalized and deduped: %#v", rule.FileTypes)
	}

	cases := []struct {
		name   string
		config map[string]any
		want   string
	}{
		{"unknown engine", map[string]any{"parser_engine_rules": []any{parserRule("nope", "pdf")}}, "not available"},
		{"unavailable engine", map[string]any{"parser_engine_rules": []any{parserRule("docreader-legacy", "pdf")}}, "not available"},
		{"unsupported type", map[string]any{"parser_engine_rules": []any{parserRule("docreader-pdf-pro", "xlsx")}}, "cannot parse"},
		{"empty rules", map[string]any{"parser_engine_rules": []any{}}, "at least one"},
		{"extra key", map[string]any{
			"parser_engine_rules": []any{parserRule("docreader-pdf-pro", "pdf")},
			"enable_multimodel":   true,
		}, "only supports parser_engine_rules"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := srv.parseImportProcessConfig(context.Background(), processConfigRequest(tc.config))
			if err == nil {
				t.Fatal("must be refused")
			}
			var toolErr *ToolError
			if !errors.As(err, &toolErr) || toolErr.Code != CodeInvalidArgument {
				t.Fatalf("expected INVALID_ARGUMENT, got %v", err)
			}
			if !strings.Contains(toolErr.Message, tc.want) {
				t.Fatalf("message must explain the refusal (%q), got %q", tc.want, toolErr.Message)
			}
		})
	}

	// 不传 process_config → nil（沿用知识库默认）
	none, err := srv.parseImportProcessConfig(context.Background(), mcp.CallToolRequest{})
	if err != nil || none != nil {
		t.Fatalf("absent process_config must stay nil: %#v %v", none, err)
	}
}

// 引擎目录不可用时不阻塞列表：本地引擎仍然返回，远端发现失败降级。
func TestListParserEnginesDegradesWhenDocreaderFails(t *testing.T) {
	srv := &Server{documentReader: &stubEngineReader{connected: true, fail: true,
		engines: []types.ParserEngineInfo{{Name: "docreader-pdf-pro", Available: true}}}}
	engines, connected := srv.listParserEngines(context.Background())
	if !connected {
		t.Fatal("connected reader must be reported")
	}
	if len(engines) == 0 {
		t.Fatal("local engines must still be listed when remote discovery fails")
	}
	for _, engine := range engines {
		if engine.Name == "docreader-pdf-pro" {
			t.Fatalf("remote engines must not appear when discovery failed: %#v", engines)
		}
	}
}
