package mcpserver

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

// 客户端只能选择明确开放的两个原生模式；未知模式必须被拒绝，不能落到任意
// agent id（内部写能力 agent 会绕过端点工具白名单）。
func TestResolveAskModeAllowsOnlyNativeModes(t *testing.T) {
	mode, agentID, err := resolveAskMode("quick-answer")
	if err != nil || mode != "quick-answer" || agentID != types.BuiltinQuickAnswerID {
		t.Fatalf("quick-answer must map to the builtin agent: %q %q %v", mode, agentID, err)
	}
	mode, agentID, err = resolveAskMode("smart-reasoning")
	if err != nil || mode != "smart-reasoning" || agentID != types.BuiltinSmartReasoningID {
		t.Fatalf("smart-reasoning must map to the builtin agent: %q %q %v", mode, agentID, err)
	}
	if mode, agentID, err := resolveAskMode("  "); err != nil || mode != "" || agentID != "" {
		t.Fatalf("empty mode must keep the endpoint default: %q %q %v", mode, agentID, err)
	}
	for _, bad := range []string{types.BuiltinWikiFixerID, "agent-own", "smart", "quick_answer"} {
		if _, _, err := resolveAskMode(bad); err == nil {
			t.Fatalf("mode %q must be refused", bad)
		}
	}
}

// 端点配置了别的 agent 时，mode 仍然映射到对应内置 agent；未传 mode 时使用端点默认。
func TestResolveAskAgentPrefersExplicitMode(t *testing.T) {
	agents := &stubAgentService{agents: map[string]*types.CustomAgent{
		types.BuiltinSmartReasoningID: {ID: types.BuiltinSmartReasoningID, TenantID: 1, IsBuiltin: true},
		"agent-own":                   {ID: "agent-own", TenantID: 1},
	}}
	srv := &Server{agentService: agents}
	ep := &types.MCPEndpoint{ID: "ep", TenantID: 1, DefaultAgentID: "agent-own"}

	got, err := srv.resolveAskAgent(context.Background(), ep, "smart-reasoning")
	if err != nil || got.ID != types.BuiltinSmartReasoningID {
		t.Fatalf("explicit mode must pin the builtin agent: %v %v", got, err)
	}
	got, err = srv.resolveAskAgent(context.Background(), ep, "")
	if err != nil || got.ID != "agent-own" {
		t.Fatalf("no mode keeps the endpoint agent: %v %v", got, err)
	}
}

func TestResolvedModeLabelReportsEndpointDefault(t *testing.T) {
	if got := resolvedModeLabel(""); got != "endpoint-default" {
		t.Fatalf("empty mode must be reported as endpoint-default, got %q", got)
	}
	if got := resolvedModeLabel("quick-answer"); got != "quick-answer" {
		t.Fatalf("mode label mismatch: %q", got)
	}
}
