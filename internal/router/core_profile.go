package router

import "github.com/Tencent/WeKnora/internal/config"

// coreProfileReachability lists the route groups a product profile registers.
//
// It is deliberately data rather than a scatter of booleans inside NewRouter:
// the knowledge-core profile is a security posture, so "which entry points are
// reachable" has to be assertable without booting gin or a database.
//
// IM delivery, web-embed publishing and public web search are no longer listed
// here: they were physically removed, so no profile registers them. The
// remaining switches still gate routes for modules that are only closed, not
// deleted yet — each switch is dropped from this table in the batch that
// removes its module.
type coreProfileReachability struct {
	// MCPClientToolbox covers the third-party MCP client surface (toolbox,
	// OAuth and approvals). The MCP SERVER surface — the main product's entry
	// point — is not part of this switch and stays reachable in both profiles.
	MCPClientToolbox bool
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
		MCPClientToolbox: true,
	}
}
