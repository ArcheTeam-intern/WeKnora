package types

import "testing"

func TestWikiWorkspaceWritePermissionIsOptIn(t *testing.T) {
	def, ok := LookupMCPEndpointTool("wiki_workspace")
	if !ok || !def.Destructive {
		t.Fatal("wiki_workspace must be a cataloged explicit write permission")
	}
	for _, name := range DefaultMCPEndpointTools() {
		if name == "wiki_workspace" {
			t.Fatal("writes must not be granted by default")
		}
	}
	caps := MCPEndpointCapabilitiesForTools([]string{"wiki_workspace"})
	if len(caps) != 1 || caps[0] != string(APIKeyCapabilityIngest) {
		t.Fatalf("write permission capabilities: %v", caps)
	}
}
