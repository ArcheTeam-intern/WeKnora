package container

import (
	"go.uber.org/dig"

	"github.com/Tencent/WeKnora/internal/config"
)

// productProfileIsKnowledgeCore reports whether this process runs the
// knowledge-core product profile.
//
// The container resolves the configuration through dig, so the profile has to
// be readable while the graph is still being built. When no configuration is
// registered (unit tests, lite construction paths) the answer is "no": the
// default profile keeps every module, which is also what makes rollback a
// configuration change rather than a code change.
func productProfileIsKnowledgeCore(c *dig.Container) bool {
	var cfg *config.Config
	if c == nil {
		return false
	}
	if err := c.Invoke(func(provided *config.Config) { cfg = provided }); err != nil {
		return false
	}
	return cfg.IsKnowledgeCore()
}
