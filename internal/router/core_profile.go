package router

import "github.com/Tencent/WeKnora/internal/config"

// coreProfileReachability lists the route groups a product profile registers.
//
// It is deliberately data rather than a scatter of booleans inside NewRouter:
// the knowledge-core profile is a security posture, so "which entry points are
// reachable" has to be assertable without booting gin or a database. The
// physical removal of the modules behind these switches is a separate, later
// batch; this type only closes reachability.
type coreProfileReachability struct {
	// IM covers the IM callback routes and the IM channel management API.
	IM bool
	// Embed covers the public embed routes and the embed channel management API.
	Embed bool
	// Memory covers the personal long-term memory API.
	Memory bool
	// PublicWebSearch covers web search and web search provider credentials.
	PublicWebSearch bool
	// MCPClientToolbox covers the third-party MCP client surface (toolbox,
	// OAuth and approvals). The MCP SERVER surface — the main product's entry
	// point — is not part of this switch and stays reachable in both profiles.
	MCPClientToolbox bool
	// ExecutionSandbox covers the agent execution sandbox: terminal, desktop,
	// local browser and the sandbox/skill configuration API.
	ExecutionSandbox bool
	// Skills covers the skill management API. Skill execution depends on the
	// sandbox, so the profile keeps them together.
	Skills bool
}

// reachabilityFor maps a configuration onto the reachable route groups. An
// unset or unknown profile behaves like full: the zero value of
// config.Config is full, which is what keeps an existing deployment and a
// rollback unchanged.
func reachabilityFor(cfg *config.Config) coreProfileReachability {
	if cfg.IsKnowledgeCore() {
		return coreProfileReachability{}
	}
	return coreProfileReachability{
		IM:               true,
		Embed:            true,
		Memory:           true,
		PublicWebSearch:  true,
		MCPClientToolbox: true,
		ExecutionSandbox: true,
		Skills:           true,
	}
}
