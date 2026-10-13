package mcpserver

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The upload receipt table has to exist in BOTH migration trees, because the
// prototype runs SQLite while the supported production databases run the
// versioned tree. A migration added to only one of them fails on the other, and
// that failure would only surface at runtime, so it is asserted here instead.

// repoRootFromTest walks up from the test's working directory to the module root
// (the directory holding go.mod).
func repoRootFromTest(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 8; i++ {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("could not locate the module root from %s", dir)
	return ""
}

func TestUploadReceiptMigrationsExistInBothTrees(t *testing.T) {
	root := repoRootFromTest(t)
	cases := []struct {
		tree    string
		pattern string
	}{
		{"migrations/sqlite", "mcp_uploads.up.sql"},
		{"migrations/sqlite", "mcp_uploads.down.sql"},
		{"migrations/versioned", "mcp_uploads.up.sql"},
		{"migrations/versioned", "mcp_uploads.down.sql"},
	}
	found := map[string]string{}
	for _, tc := range cases {
		dir := filepath.Join(root, tc.tree)
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), tc.pattern) {
				found[tc.tree+"/"+tc.pattern] = filepath.Join(dir, entry.Name())
			}
		}
		if _, ok := found[tc.tree+"/"+tc.pattern]; !ok {
			t.Errorf("%s has no *%s migration", tc.tree, tc.pattern)
		}
	}
	for key, path := range found {
		if strings.Contains(key, ".down.") {
			continue
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		sql := string(body)
		// Every column the repository writes must exist in the migration: a
		// mismatch here is a runtime "no such column", which is much harder to
		// diagnose than a failing test.
		for _, column := range []string{
			"id", "endpoint_id", "caller_workspace_id", "caller_principal_storage_id",
			"owner_tenant_id", "knowledge_base_id", "idempotency_key", "file_name",
			"file_size", "sha256", "state", "document_id", "deduplicated", "queued",
			"last_error_code", "last_error_message", "lease_token", "lease_expires_at",
			"created_at", "expires_at", "receipt_expires_at",
		} {
			if !regexp.MustCompile(`(?m)^\s*` + column + `\s`).MatchString(sql) {
				t.Errorf("%s is missing column %q", path, column)
			}
		}
		if !strings.Contains(sql, "idx_mcp_uploads_idempotency") {
			t.Errorf("%s is missing the idempotency unique index", path)
		}
		if !strings.Contains(sql, "idempotency_key <> ''") {
			t.Errorf("%s must scope the idempotency index to non-empty keys, "+
				"otherwise every keyless upload collides on one row", path)
		}
	}
}

// The down migration must not destroy knowledge: it may only drop the receipt
// table and its indexes.
func TestUploadReceiptDownMigrationIsScoped(t *testing.T) {
	root := repoRootFromTest(t)
	for _, tree := range []string{"migrations/sqlite", "migrations/versioned"} {
		dir := filepath.Join(root, tree)
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		var body string
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), "mcp_uploads.down.sql") {
				raw, readErr := os.ReadFile(filepath.Join(dir, entry.Name()))
				if readErr != nil {
					t.Fatalf("read %s: %v", entry.Name(), readErr)
				}
				body = strings.ToLower(string(raw))
			}
		}
		if body == "" {
			t.Fatalf("%s: no mcp_uploads down migration", tree)
		}
		for _, forbidden := range []string{"drop table if exists knowledges", "drop table if exists chunks",
			"drop table if exists knowledge_bases", "drop table if exists wiki_pages"} {
			if strings.Contains(body, forbidden) {
				t.Errorf("%s drops knowledge data (%q); a rollback must only remove upload receipts",
					tree, forbidden)
			}
		}
	}
}
