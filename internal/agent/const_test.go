package agent

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// Shell and browser tools carried longer caps; both are retired with the
// execution sandbox, so every tool gets the default execution timeout.
func TestToolExecutionTimeout(t *testing.T) {
	assert.Equal(t, 60*time.Second, toolExecutionTimeout("shell_exec"))
	assert.Equal(t, 60*time.Second, toolExecutionTimeout("web_fetch"))
	assert.Equal(t, 60*time.Second, toolExecutionTimeout("local_browser", `{"method":"request_help"}`))
	assert.Equal(t, 60*time.Second, toolExecutionTimeout("search_knowledge"))
}
