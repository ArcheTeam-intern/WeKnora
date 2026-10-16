package database

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// TestLegacyCompatTablesPreservedWithoutWriters pins Task 5 of the pruning
// plan: every non-core table the deletion batches kept stays in the schema
// after a fresh migration, and the legacy operational tables
// (tenant_skill_*, tenant_sandbox_config_*, mcp_oauth_tokens,
// mcp_tool_approvals) stay readable. New code paths no longer write to
// them — that is checked separately by a code scan.
func TestLegacyCompatTablesPreservedWithoutWriters(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	previousDir, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(repoRoot))
	t.Cleanup(func() { _ = os.Chdir(previousDir) })

	dbPath := filepath.Join(t.TempDir(), "legacy.db")
	require.NoError(t, RunMigrationsWithOptions("sqlite3://unused", MigrationOptions{SQLiteDBPath: dbPath}))

	db, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	rows, err := sqlDB.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`)
	require.NoError(t, err)
	defer rows.Close()
	existing := make(map[string]bool)
	for rows.Next() {
		var n string
		require.NoError(t, rows.Scan(&n))
		existing[n] = true
	}

	mustKeep := []string{
		"sessions", "messages", "custom_agents", "knowledges", "chunks",
		"wiki_pages", "mcp_endpoints", "users", "tenants", "tenant_members",
		"tenant_skills", "tenant_skill_snapshots", "tenant_sandbox_configs",
		"mcp_oauth_tokens", "mcp_tool_approvals",
		"fork_snapshot_leases",
	}
	for _, table := range mustKeep {
		require.Truef(t, existing[table],
			"Task 5 rule: legacy non-core table %q must remain in the schema (only-stop-write, no DROP)", table)
	}
}

// TestLegacyCompatTablesReceiveNoNewWrites is the static side of the Task 5
// rule: production code no longer INSERTs into the legacy non-core tables.
// The migration directory is excluded because migrations seed those tables
// on the way up; test files are excluded because tests legitimately insert
// rows to assert behaviour. GORM column tags containing the legacy table
// names (e.g. `gorm:"uniqueIndex:idx_mcp_oauth_tokens_..."`) are also out
// of scope: only the SQL `INSERT INTO <table>` form is matched.
func TestLegacyCompatTablesReceiveNoNewWrites(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)

	legacyTables := []string{
		"tenant_skills", "tenant_skill_snapshots", "tenant_skill_files",
		"tenant_sandbox_configs", "sandbox_configs",
		"mcp_oauth_tokens", "mcp_tool_approvals",
		"web_search_providers", "web_search_configs",
	}
	// Match `INSERT [IGNORE/OR ABORT/REPLACE]? INTO <table>` written as SQL:
	// the previous character is whitespace or start-of-line, and the table
	// name is followed by a non-identifier character. That excludes GORM
	// struct tags like `gorm:"...uniqueIndex:idx_mcp_oauth_tokens_..."`
	// where the underscore inside the index name is a legal identifier
	// character and there is no `INSERT` keyword anywhere nearby.
	insertRe := regexp.MustCompile(
		`(?m)(?:^|\s)INSERT\s+(?:IGNORE\s+|OR\s+ABORT\s+|REPLACE\s+)?INTO\s+` +
			`?` + `(?:` + strings.Join(legacyTables, `|`) + `)\b`,
	)

	excluded := map[string]bool{
		"migrations":                 true,
		"internal/database":          true,
		"internal/database/testdata": true,
	}

	var hits []string
	_ = filepath.Walk(repoRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			for dir := range excluded {
				if path == filepath.Join(repoRoot, dir) {
					return filepath.SkipDir
				}
			}
			if strings.HasSuffix(path, "_test.go") {
				return nil
			}
			if !strings.Contains(path, string(filepath.Separator)+"internal"+string(filepath.Separator)) &&
				!strings.Contains(path, string(filepath.Separator)+"cmd"+string(filepath.Separator)) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		if loc := insertRe.FindStringIndex(string(b)); loc != nil {
			rel, _ := filepath.Rel(repoRoot, path)
			hits = append(hits, rel)
		}
		return nil
	})

	require.Emptyf(t, hits,
		"Task 5 rule: production code must not INSERT into legacy non-core tables; "+
			"offending files: %v", hits)
}
