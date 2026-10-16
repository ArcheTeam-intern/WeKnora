package types

import (
	"testing"
)

func TestResolveSystemPrompt(t *testing.T) {
	tests := []struct {
		name   string
		config *AgentConfig
		want   string
	}{
		{
			name:   "nil config returns empty",
			config: nil,
			want:   "",
		},
		{
			name:   "unified SystemPrompt returned",
			config: &AgentConfig{SystemPrompt: "my prompt"},
			want:   "my prompt",
		},
		{
			// Public web search is retired: the deprecated "web enabled" field
			// is never selected, the web-disabled one is the fallback.
			name: "deprecated fallback picks the web-disabled variant",
			config: &AgentConfig{
				SystemPromptWebDisabled: "no-web",
				SystemPromptWebEnabled:  "with-web",
			},
			want: "no-web",
		},
		{
			name:   "empty config returns empty",
			config: &AgentConfig{},
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.config.ResolveSystemPrompt()
			if got != tt.want {
				t.Errorf("ResolveSystemPrompt() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestUseCustomSystemPromptFlag verifies that UseCustomSystemPrompt=false
// does not block a non-empty SystemPrompt from being resolved by agent_service.
// The fix in agent_service.go checks config.SystemPrompt != "" as a fallback,
// so ResolveSystemPrompt must return the prompt even when the flag is false.
func TestUseCustomSystemPromptFlag(t *testing.T) {
	cfg := &AgentConfig{
		SystemPrompt:          "custom prompt",
		UseCustomSystemPrompt: false, // flag NOT set
	}

	// ResolveSystemPrompt should still return the prompt
	got := cfg.ResolveSystemPrompt()
	if got != "custom prompt" {
		t.Errorf("expected 'custom prompt', got %q", got)
	}
}

// The window the agent plans against must come from the model when the model
// knows it. Assuming a window wider than the real one means compaction never
// runs and the provider rejects the request instead.
func TestAgentMaxContextTokens(t *testing.T) {
	if got := AgentMaxContextTokens(0, 0); got != DefaultMaxContextTokens {
		t.Fatalf("nothing known = %d, want %d", got, DefaultMaxContextTokens)
	}
	if got := AgentMaxContextTokens(0, 32768); got != 32768 {
		t.Fatalf("model window unused: got %d, want 32768", got)
	}
	if got := AgentMaxContextTokens(64000, 32768); got != 64000 {
		t.Fatalf("explicit agent setting must win, got %d", got)
	}
	if got := AgentMaxContextTokens(64000, 0); got != 64000 {
		t.Fatalf("explicit agent setting with unknown model = %d, want 64000", got)
	}
}

func TestAgentRoundMaxCompletionTokens(t *testing.T) {
	if got := AgentRoundMaxCompletionTokens(0); got != DefaultSmartReasoningMaxCompletionTokens {
		t.Fatalf("zero configured = %d, want %d", got, DefaultSmartReasoningMaxCompletionTokens)
	}
	if got := AgentRoundMaxCompletionTokens(2048); got != 2048 {
		t.Fatalf("explicit 2048 = %d, want 2048", got)
	}
	if got := AgentRoundMaxCompletionTokens(4096); got != 4096 {
		t.Fatalf("explicit 4096 = %d, want 4096", got)
	}
	// The sandbox write-file floor (24576 with a bound sandbox, 8192 for an
	// explicit small cap) went with the execution sandbox: no tool carries a
	// file body in its JSON arguments any more.
	if got := AgentRoundMaxCompletionTokens(64000); got != 64000 {
		t.Fatalf("explicit 64000 = %d, want 64000", got)
	}
}
