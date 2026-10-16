package container

import (
	"testing"

	"go.uber.org/dig"

	"github.com/Tencent/WeKnora/internal/config"
)

func TestProductProfileIsKnowledgeCoreReadsTheContainerConfig(t *testing.T) {
	core := dig.New()
	if err := core.Provide(func() *config.Config {
		return &config.Config{ProductProfile: config.ProductProfileKnowledgeCore}
	}); err != nil {
		t.Fatalf("provide core config: %v", err)
	}
	if !productProfileIsKnowledgeCore(core) {
		t.Fatal("a knowledge-core configuration must be detected")
	}

	full := dig.New()
	if err := full.Provide(func() *config.Config {
		return &config.Config{}
	}); err != nil {
		t.Fatalf("provide default config: %v", err)
	}
	if productProfileIsKnowledgeCore(full) {
		t.Fatal("the default profile must not be treated as knowledge-core")
	}
}

// A container without a registered configuration (unit tests, degraded
// construction) must keep the full behaviour instead of guessing, and a nil
// container must not panic.
func TestProductProfileDefaultsToFullWithoutConfiguration(t *testing.T) {
	if productProfileIsKnowledgeCore(dig.New()) {
		t.Fatal("a container without configuration must default to full")
	}
	if productProfileIsKnowledgeCore(nil) {
		t.Fatal("a nil container must default to full")
	}
}
