package database

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// Task 5 of the backend pruning plan retires a set of non-core tables by
// stopping every writer while leaving the tables and their rows in place: no
// DROP, no purge, no volume rebuild. The tests in this file pin that promise
// from four sides:
//
//  1. TestLegacyCompatTablesPreservedWithoutWriters — a fresh migration still
//     creates the tables.
//  2. TestLegacyCompatTablesReceiveNoNewWrites — the SQL form: production code
//     contains no `INSERT INTO <retired table>` literal.
//  3. TestLegacyCompatTablesHaveNoRetiredORMWriters — the ORM form: the retired
//     MCP OAuth / tool-approval persistence contracts stay declaration-only,
//     so no GORM writer can be wired for them.
//  4. TestLegacyCompatTablesPreserveOldRowsAfterUpgrade — rows written before
//     the pruning survive the migration path, and core rows stay usable.
//
// Coverage limits, stated plainly: the scans are text-based over the
// production trees (internal/, cmd/) and the migration sources. A write whose
// statement is assembled at runtime from a table-name variable, and a write
// performed by code that is unreachable but still compiled, are out of scope.

// legacyNonCoreTables are the tables the deletion batches retired. They are
// matched as whole SQL identifiers; "sandbox_configs", "tenant_skill_files",
// "web_search_providers" and "web_search_configs" are names from the retired
// feature set that are listed so an accidental revival of any of them is
// flagged the same way.
var legacyNonCoreTables = []string{
	"tenant_skills", "tenant_skill_snapshots", "tenant_skill_files",
	"tenant_sandbox_configs", "sandbox_configs",
	"mcp_oauth_tokens", "mcp_tool_approvals",
	"web_search_providers", "web_search_configs",
}

// retiredTableAlternation is the regex alternation shared by the INSERT and
// DROP scanners.
var retiredTableAlternation = strings.Join(legacyNonCoreTables, `|`)

// legacyInsertRe matches the SQL text form of a write into a retired table:
// INSERT [IGNORE|OR ABORT|REPLACE] INTO <quote?> <table>. The character before
// the keyword may be whitespace or the opening quote of a Go string literal —
// an earlier revision required whitespace and therefore missed the most common
// production spelling, db.Exec("INSERT INTO ..."). The trailing \b keeps a
// retired name from matching a longer table name.
var legacyInsertRe = regexp.MustCompile(
	`(?im)(?:^|[\s"'` + "`" + `])INSERT\s+(?:IGNORE\s+|OR\s+ABORT\s+|REPLACE\s+)?INTO\s+["` + "`" + `]?(?:` +
		retiredTableAlternation + `)\b`,
)

// legacyDropRe matches a DROP TABLE of a retired table, used to check the
// applied migration sources. Down migrations are out of scope: the up path
// never runs them.
var legacyDropRe = regexp.MustCompile(
	`(?i)\bDROP\s+TABLE\s+(?:IF\s+EXISTS\s+)?["` + "`" + `]?(?:` + retiredTableAlternation + `)\b`,
)

// retiredWriterMethods are the write methods of the retired MCP OAuth and
// tool-approval contracts. No production type may implement them: that
// implementation is exactly the ORM write path the SQL scan cannot see.
// Method names that a core type also uses are deliberately absent —
// authTokenRepository.DeleteToken is a core method and must not trip the
// guard, and the generic service setters are covered by the contract-name
// check because an implementation of them has to name the retired contract.
var retiredWriterMethods = []string{
	"SaveClient", "DeleteClient",
	"SaveToken", "SaveTokenForPrincipal", "DeleteTokenForPrincipal",
	"TryAcquireTokenRefreshLease", "ReleaseTokenRefreshLease",
	"UpsertPolicy",
}

// retiredWriterRe matches a method implementation of a retired writer.
var retiredWriterRe = regexp.MustCompile(
	`(?m)^func \([^)]*\) (` + strings.Join(retiredWriterMethods, `|`) + `)\(`,
)

// retiredTableRefRe matches a GORM Table() call naming a retired table
// directly. On its own it is not a violation — the tables stay readable — so
// the write decision is made by walking the method chain that follows it.
var retiredTableRefRe = regexp.MustCompile(
	`\.Table\(\s*["` + "`" + `]?(?:` + retiredTableAlternation + `)["` + "`" + `]?\s*\)`,
)

// gormChainLink parses one link of a fluent GORM chain at the start of rest:
// optional whitespace, a dot, a method name and a balanced parenthesised
// argument list. It returns the method name and the number of bytes consumed,
// with string literals skipped so a quote inside an argument cannot unbalance
// the scan.
func gormChainLink(rest string) (string, int, bool) {
	isSpace := func(b byte) bool { return b == ' ' || b == '\t' || b == '\n' || b == '\r' }
	isName := func(b byte) bool {
		return b == '_' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
	}
	i := 0
	for i < len(rest) && isSpace(rest[i]) {
		i++
	}
	if i >= len(rest) || rest[i] != '.' {
		return "", 0, false
	}
	i++
	start := i
	for i < len(rest) && isName(rest[i]) {
		i++
	}
	if i == start {
		return "", 0, false
	}
	name := rest[start:i]
	for i < len(rest) && isSpace(rest[i]) {
		i++
	}
	if i >= len(rest) || rest[i] != '(' {
		return "", 0, false
	}
	depth := 0
	for i < len(rest) {
		switch rest[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return name, i + 1, true
			}
		case '"', '`', '\'':
			quote := rest[i]
			i++
			for i < len(rest) {
				if rest[i] == '\\' && quote != '`' {
					i += 2
					continue
				}
				if rest[i] == quote {
					break
				}
				i++
			}
		}
		i++
	}
	return "", 0, false
}

// gormWriterMethods are the GORM chain methods that write rows. Reads
// (Count, Find, First, Pluck, Scan, ...) are deliberately absent: reading the
// retired tables is allowed by design. The classification is by the executed
// method: Unscoped is deliberately NOT a writer here — it is a chain
// modifier, and a read through it (Table("x").Unscoped().Count(&n)) stays a
// read, while a write through it (Table("x").Unscoped().Delete(...)) is
// still caught by the terminal writer that follows.
var gormWriterMethods = map[string]struct{}{
	"Create": {}, "CreateInBatches": {}, "Save": {}, "Updates": {}, "Update": {},
	"UpdateColumn": {}, "UpdateColumns": {}, "FirstOrCreate": {}, "Assign": {},
	"Delete": {},
}

// retiredTableSQLExecRe matches a raw write statement naming a retired table
// directly, which the INSERT scan misses when the verb is UPDATE or DELETE.
var retiredTableSQLExecRe = regexp.MustCompile(
	`(?i)\b(?:UPDATE|DELETE\s+FROM|REPLACE\s+INTO)\s+["` + "`" + `]?(?:` + retiredTableAlternation + `)\b`,
)

// retiredTableORMWriters returns the write chains in content that open on a
// retired table by name, e.g. Table("mcp_tool_approvals").Create(row) or
// Table("tenant_skins").Where(...).Updates(...). The chain is walked link by
// link rather than matched by one flat pattern, so an intermediate call does
// not hide the write, and a read chain (Table("x").Count(...)) is not
// reported.
//
// Coverage limit, stated plainly: this is a text scan. A table name assembled
// at runtime (a variable, a helper, fmt.Sprintf) or a model whose TableName()
// returns a retired table without the literal appearing at the call site is
// out of scope.
func retiredTableORMWriters(content string) []string {
	var hits []string
	for _, loc := range retiredTableRefRe.FindAllStringIndex(content, -1) {
		rest := content[loc[1]:]
		chain := content[loc[0]:loc[1]]
		writes := false
		consumed := 0
		for {
			name, length, ok := gormChainLink(rest[consumed:])
			if !ok {
				break
			}
			consumed += length
			chain += rest[consumed-length : consumed]
			if _, isWriter := gormWriterMethods[name]; isWriter {
				writes = true
			}
		}
		if writes {
			hits = append(hits, strings.Join(strings.Fields(chain), " "))
		}
	}
	return hits
}

// TestLegacyCompatTablesPreservedWithoutWriters pins the schema side of Task
// 5: every non-core table the deletion batches kept stays in the schema after
// a fresh migration, and the legacy operational tables (tenant_skill_*,
// tenant_sandbox_configs, mcp_oauth_tokens, mcp_tool_approvals) stay
// readable. New code paths no longer write to them — that is checked by the
// scans below.
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
// rule: production code no longer INSERTs into the retired tables. The scan
// itself is exercised on a fixture tree first, so a scanner that reads nothing
// cannot pass by accident: the fixture tree must yield its production files,
// the two SQL violations in it must be flagged, the ORM write and the _test.go
// INSERT must not, and an unreadable file must fail the scan.
func TestLegacyCompatTablesReceiveNoNewWrites(t *testing.T) {
	fixtureRoot := t.TempDir()
	writeFixtureFile(t, fixtureRoot, "internal/service/quoted_insert.go", `package service

import "gorm.io/gorm"

func quotedInsert(db *gorm.DB, id string) {
	db.Exec("INSERT INTO tenant_skills (id) VALUES (?)", id)
}
`)
	// The raw-string spelling puts INSERT on its own line, the spelling the
	// first scanner revision did catch.
	writeFixtureFile(t, fixtureRoot, "internal/service/raw_insert.go",
		"package service\n\nimport \"gorm.io/gorm\"\n\nconst rawInsert = `\nINSERT INTO tenant_sandbox_configs (id) VALUES (?)\n`\n\nfunc rawInsertExec(db *gorm.DB) {\n\tdb.Exec(rawInsert)\n}\n")
	// Naming a retired table is not a write: reads are still allowed.
	writeFixtureFile(t, fixtureRoot, "internal/service/legacy_read.go", `package service

import "gorm.io/gorm"

func countLegacy(db *gorm.DB) int64 {
	var n int64
	db.Table("tenant_skills").Count(&n)
	return n
}
`)
	// An ORM write is invisible to the SQL scan on purpose; it is the reason
	// the ORM contract check below exists.
	writeFixtureFile(t, fixtureRoot, "internal/service/orm_write.go", `package service

import "gorm.io/gorm"

type approval struct {
	ID string
}

func ormWrite(db *gorm.DB, row *approval) {
	db.Table("mcp_tool_approvals").Create(row)
}
`)
	// Tests legitimately insert rows; _test.go files must not be collected.
	writeFixtureFile(t, fixtureRoot, "internal/service/legacy_seed_test.go", `package service

import "gorm.io/gorm"

func seedLegacy(db *gorm.DB) {
	db.Exec("INSERT INTO tenant_skills (id) VALUES ('seed')")
}
`)
	writeFixtureFile(t, fixtureRoot, "cmd/app/main.go", "package main\n\nfunc main() {}\n")

	fixtureFiles, err := collectProductionGoFiles(fixtureRoot)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{
		filepath.Join(fixtureRoot, "cmd", "app", "main.go"),
		filepath.Join(fixtureRoot, "internal", "service", "legacy_read.go"),
		filepath.Join(fixtureRoot, "internal", "service", "orm_write.go"),
		filepath.Join(fixtureRoot, "internal", "service", "quoted_insert.go"),
		filepath.Join(fixtureRoot, "internal", "service", "raw_insert.go"),
	}, fixtureFiles, "fixture walk must collect production .go files and skip _test.go")

	fixtureHits, err := scanFilesForMatches(fixtureFiles, legacyInsertRe)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{
		filepath.Join(fixtureRoot, "internal", "service", "quoted_insert.go"),
		filepath.Join(fixtureRoot, "internal", "service", "raw_insert.go"),
	}, fixtureHits, "fixture scan must flag the SQL INSERTs and nothing else")

	_, err = scanFilesForMatches([]string{filepath.Join(fixtureRoot, "internal", "service", "missing.go")}, legacyInsertRe)
	require.Error(t, err, "an unreadable file must fail the scan instead of being skipped")

	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	files, err := collectProductionGoFiles(repoRoot)
	require.NoError(t, err)
	require.Greaterf(t, len(files), 100,
		"Task 5 scanner must actually walk the production trees; it collected %d files", len(files))
	var fromInternal, fromCmd int
	for _, p := range files {
		switch {
		case strings.HasPrefix(p, filepath.Join(repoRoot, "internal")+string(filepath.Separator)):
			fromInternal++
		case strings.HasPrefix(p, filepath.Join(repoRoot, "cmd")+string(filepath.Separator)):
			fromCmd++
		}
	}
	require.Greater(t, fromInternal, 100, "expected production files under internal/")
	require.Greater(t, fromCmd, 0, "expected production files under cmd/")
	require.Equal(t, len(files), fromInternal+fromCmd,
		"the scanner must only collect files below internal/ and cmd/")

	hits, err := scanFilesForMatches(files, legacyInsertRe)
	require.NoError(t, err)
	require.Emptyf(t, hits,
		"Task 5 rule: production code must not INSERT into retired tables; offending files: %v", hits)
}

// TestLegacyCompatTablesHaveNoRetiredORMWriters covers the write forms the SQL
// INSERT scan cannot see. Three checks stand in for it:
//
//  1. no production type implements the retired write methods,
//  2. the retired persistence contracts are named only in their declaration
//     files (either one is the wiring an ORM writer needs), and
//  3. no production code opens a write chain on a retired table by name —
//     db.Table("mcp_tool_approvals").Create(row) names no contract and
//     contains no SQL, and it is exactly how the reviewer bypassed the first
//     two checks.
//
// Coverage limit, stated plainly: the table-name scan is a text scan, so a
// write whose table name is assembled at runtime (a variable, a helper,
// fmt.Sprintf) is out of scope, as is a model whose TableName() returns a
// retired table without the name appearing at the call site. Reads are
// allowed on purpose — the retired tables stay readable.
func TestLegacyCompatTablesHaveNoRetiredORMWriters(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	files, err := collectProductionGoFiles(repoRoot)
	require.NoError(t, err)
	require.NotEmpty(t, files)

	// The scanner itself is exercised on a fixture tree first: a table-name
	// write chain must be flagged, a read must not, and the pre-fix bypass
	// shape must be caught even though it names no contract.
	fixtureRoot := t.TempDir()
	writeFixtureFile(t, fixtureRoot, "internal/service/table_create.go", `package service

import "gorm.io/gorm"

type approval struct{ ID string }

func tableCreate(db *gorm.DB, row *approval) {
	db.Table("mcp_tool_approvals").Create(row)
}
`)
	writeFixtureFile(t, fixtureRoot, "internal/service/table_updates.go", `package service

import "gorm.io/gorm"

func tableUpdates(db *gorm.DB) {
	db.Table("tenant_skill_snapshots").Where("id = ?", "x").Updates(map[string]any{"state": "ready"})
}
`)
	writeFixtureFile(t, fixtureRoot, "internal/service/raw_update.go", `package service

import "gorm.io/gorm"

func rawUpdate(db *gorm.DB) {
	db.Exec("UPDATE tenant_sandbox_configs SET name = ? WHERE id = ?", "n", "x")
}
`)
	// Reads must stay allowed: the tables remain readable by design.
	writeFixtureFile(t, fixtureRoot, "internal/service/table_read.go", `package service

import "gorm.io/gorm"

func tableRead(db *gorm.DB) int64 {
	var n int64
	db.Table("tenant_skills").Count(&n)
	return n
}
`)
	// A model write that names no retired table textually: out of scope of
	// this scan (the doc comment says so), so it must NOT be flagged.
	writeFixtureFile(t, fixtureRoot, "internal/service/model_write.go", `package service

import "gorm.io/gorm"

type row struct{ ID string }

func modelWrite(db *gorm.DB, r *row) {
	db.Model(&row{}).Create(r)
}
`)
	// Unscoped is a chain modifier, not an executed write: a read through
	// it must stay unflagged (the review's P3 counterexample).
	writeFixtureFile(t, fixtureRoot, "internal/service/unscoped_read.go", `package service

import "gorm.io/gorm"

func unscopedCount(db *gorm.DB) int64 {
	var n int64
	db.Table("tenant_skills").Unscoped().Count(&n)
	return n
}
`)
	// A write executed THROUGH Unscoped must still be flagged — the terminal
	// method decides, and removing the modifier from the writer set must not
	// have opened that hole.
	writeFixtureFile(t, fixtureRoot, "internal/service/unscoped_delete.go", `package service

import "gorm.io/gorm"

func unscopedDelete(db *gorm.DB) {
	db.Table("mcp_tool_approvals").Unscoped().Delete(nil, "id = ?", "x")
}
`)
	writeFixtureFile(t, fixtureRoot, "cmd/app/main.go", "package main\n\nfunc main() {}\n")

	fixtureFiles, err := collectProductionGoFiles(fixtureRoot)
	require.NoError(t, err)
	require.Len(t, fixtureFiles, 8)

	fixtureHits := scanFilesForORMWrites(t, fixtureFiles)
	require.ElementsMatch(t, []string{
		filepath.Join(fixtureRoot, "internal", "service", "table_create.go"),
		filepath.Join(fixtureRoot, "internal", "service", "table_updates.go"),
		filepath.Join(fixtureRoot, "internal", "service", "unscoped_delete.go"),
	}, fixtureHits, "the fixture's table-name write chains must be flagged, and nothing else")

	execHits, err := scanFilesForMatches(fixtureFiles, retiredTableSQLExecRe)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{filepath.Join(fixtureRoot, "internal", "service", "raw_update.go")},
		execHits, "a raw UPDATE against a retired table must be flagged even though it is not an INSERT")

	retiredContracts := []string{"MCPOAuthRepository", "MCPToolApprovalRepository", "MCPToolApprovalService"}
	declarationFiles := map[string]bool{
		filepath.Join("internal", "types", "interfaces", "mcp_oauth.go"):         true,
		filepath.Join("internal", "types", "interfaces", "mcp_tool_approval.go"): true,
	}

	var offenders []string
	var writerOffenders []string
	var tableWriterOffenders []string
	var seenDeclarations int
	for _, p := range files {
		b, err := os.ReadFile(p)
		require.NoErrorf(t, err, "read %s", p)
		content := string(b)

		rel, err := filepath.Rel(repoRoot, p)
		require.NoError(t, err)

		for _, match := range retiredWriterRe.FindAllString(content, -1) {
			writerOffenders = append(writerOffenders, fmt.Sprintf("%s: %s", rel, strings.TrimSpace(match)))
		}
		for _, match := range retiredTableORMWriters(content) {
			tableWriterOffenders = append(tableWriterOffenders, fmt.Sprintf("%s: %s", rel, match))
		}
		for _, match := range retiredTableSQLExecRe.FindAllString(content, -1) {
			tableWriterOffenders = append(tableWriterOffenders,
				fmt.Sprintf("%s: %s", rel, strings.Join(strings.Fields(match), " ")))
		}

		var mentions []string
		for _, name := range retiredContracts {
			if strings.Contains(content, name) {
				mentions = append(mentions, name)
			}
		}
		if len(mentions) == 0 {
			continue
		}
		if declarationFiles[rel] {
			seenDeclarations++
			continue
		}
		offenders = append(offenders, fmt.Sprintf("%s (%s)", rel, strings.Join(mentions, ", ")))
	}
	require.Emptyf(t, writerOffenders,
		"Task 5 rule: no production type may implement the retired writer methods "+
			"(the ORM write path the SQL scan cannot see); offending declarations: %v", writerOffenders)
	require.Emptyf(t, tableWriterOffenders,
		"Task 5 rule: no production code may write to a retired table by name "+
			"(db.Table(\"<retired>\").Create/Save/Updates, or a raw UPDATE/DELETE); "+
			"offending call sites: %v", tableWriterOffenders)
	require.Emptyf(t, offenders,
		"Task 5 rule: retired MCP persistence contracts must stay declaration-only; "+
			"offending files: %v", offenders)
	require.Equal(t, len(declarationFiles), seenDeclarations,
		"expected the retired contracts to still be declared in the interface files")
}

// TestLegacyCompatTablesPreserveOldRowsAfterUpgrade seeds the retired tables
// with rows an older installation would already hold, then runs the migration
// path again — what an in-place upgrade of the server does at startup — and
// asserts that every row and its content survived, that core rows
// (tenant / knowledge base / knowledge / session) still read and write, and
// that the core writes did not land in a retired table.
//
// The fixture is not a version-bump replay: the database is already at the
// recorded version, so the second migration run applies nothing and only
// proves the upgrade path completes cleanly against a database that holds
// data. The static half of "rows survive" — no applied migration drops a
// retired table — is checked directly against the migration sources. The core
// writes here are raw SQL standing in for the rows the product's repositories
// produce, not a repository code path.
func TestLegacyCompatTablesPreserveOldRowsAfterUpgrade(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	previousDir, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(repoRoot))
	t.Cleanup(func() { _ = os.Chdir(previousDir) })

	// Static half: no applied migration may DROP a retired table.
	upMigrations, err := filepath.Glob(filepath.Join(repoRoot, "migrations", "*", "*.up.sql"))
	require.NoError(t, err)
	require.NotEmpty(t, upMigrations, "expected migration sources to exist")
	dropHits, err := scanFilesForMatches(upMigrations, legacyDropRe)
	require.NoError(t, err)
	require.Emptyf(t, dropHits,
		"Task 5 rule: no applied migration may DROP a retired table; offending files: %v", dropHits)

	dbPath := filepath.Join(t.TempDir(), "upgrade.db")
	require.NoError(t, RunMigrationsWithOptions("sqlite3://unused", MigrationOptions{SQLiteDBPath: dbPath}))

	sqlDB, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	scalar := func(query string) string {
		var got string
		require.NoErrorf(t, sqlDB.QueryRow(query).Scan(&got), "query %s", query)
		return got
	}
	countRows := func(table string) int {
		var n int
		require.NoErrorf(t, sqlDB.QueryRow("SELECT COUNT(*) FROM "+table).Scan(&n), "count %s", table)
		return n
	}
	exec := func(insert string) {
		_, err := sqlDB.Exec(insert)
		require.NoErrorf(t, err, "exec %s", insert)
	}

	// Rows written by the pre-pruning product: they must survive the upgrade.
	retiredRows := []struct {
		table  string
		insert string
		verify string
		want   string
	}{
		{
			table: "tenant_skills",
			insert: `INSERT INTO tenant_skills (id, tenant_id, sandbox_config_id, name, status) ` +
				`VALUES ('skill-legacy-1', 1, 'sandbox-legacy-1', 'legacy-skill', 'ready')`,
			verify: `SELECT name FROM tenant_skills WHERE id = 'skill-legacy-1'`,
			want:   "legacy-skill",
		},
		{
			table: "tenant_skill_snapshots",
			insert: `INSERT INTO tenant_skill_snapshots (id, tenant_id, sandbox_config_id, trigger, state) ` +
				`VALUES ('snapshot-legacy-1', 1, 'sandbox-legacy-1', 'install', 'ready')`,
			verify: `SELECT state FROM tenant_skill_snapshots WHERE id = 'snapshot-legacy-1'`,
			want:   "ready",
		},
		{
			table: "tenant_sandbox_configs",
			insert: `INSERT INTO tenant_sandbox_configs (id, tenant_id, name, sandbox_type, config) ` +
				`VALUES ('sandbox-legacy-1', 1, 'legacy-sandbox', 'docker', '{"image":"legacy"}')`,
			verify: `SELECT config FROM tenant_sandbox_configs WHERE id = 'sandbox-legacy-1'`,
			want:   `{"image":"legacy"}`,
		},
		{
			table: "mcp_oauth_tokens",
			insert: `INSERT INTO mcp_oauth_tokens (id, tenant_id, user_id, service_id, access_token) ` +
				`VALUES ('token-legacy-1', 1, 'user-legacy-1', 'service-legacy-1', 'legacy-access-token')`,
			verify: `SELECT access_token FROM mcp_oauth_tokens WHERE id = 'token-legacy-1'`,
			want:   "legacy-access-token",
		},
		{
			table: "mcp_tool_approvals",
			insert: `INSERT INTO mcp_tool_approvals (id, tenant_id, service_id, tool_name, require_approval) ` +
				`VALUES ('approval-legacy-1', 1, 'service-legacy-1', 'legacy_tool', 1)`,
			verify: `SELECT tool_name FROM mcp_tool_approvals WHERE id = 'approval-legacy-1'`,
			want:   "legacy_tool",
		},
	}
	for _, row := range retiredRows {
		exec(row.insert)
	}

	// Core rows the same installation holds: knowledge base, document, session.
	coreRows := []struct {
		table  string
		insert string
		verify string
		want   string
	}{
		{
			table:  "tenants",
			insert: `INSERT INTO tenants (id, name, business) VALUES (1, 'legacy-tenant', 'legacy')`,
			verify: `SELECT name FROM tenants WHERE id = 1`,
			want:   "legacy-tenant",
		},
		{
			table: "knowledge_bases",
			insert: `INSERT INTO knowledge_bases (id, name, tenant_id, embedding_model_id, summary_model_id) ` +
				`VALUES ('kb-legacy-1', 'legacy-kb', 1, 'embed-legacy', 'summary-legacy')`,
			verify: `SELECT name FROM knowledge_bases WHERE id = 'kb-legacy-1'`,
			want:   "legacy-kb",
		},
		{
			table: "knowledges",
			insert: `INSERT INTO knowledges (id, tenant_id, knowledge_base_id, type, title, source) ` +
				`VALUES ('doc-legacy-1', 1, 'kb-legacy-1', 'file', 'legacy-doc.pdf', 'legacy-doc.pdf')`,
			verify: `SELECT title FROM knowledges WHERE id = 'doc-legacy-1'`,
			want:   "legacy-doc.pdf",
		},
		{
			table: "sessions",
			insert: `INSERT INTO sessions (id, tenant_id, title, user_id, knowledge_base_id) ` +
				`VALUES ('session-legacy-1', 1, 'legacy-session', 'user-legacy-1', 'kb-legacy-1')`,
			verify: `SELECT title FROM sessions WHERE id = 'session-legacy-1'`,
			want:   "legacy-session",
		},
	}
	for _, row := range coreRows {
		exec(row.insert)
	}

	// The upgrade: the new build runs its migration path against the database
	// that already holds that data.
	require.NoError(t, RunMigrationsWithOptions("sqlite3://unused", MigrationOptions{SQLiteDBPath: dbPath}))

	var version int
	var dirty int
	require.NoError(t, sqlDB.QueryRow("SELECT version, dirty FROM schema_migrations").Scan(&version, &dirty))
	require.Zero(t, dirty, "the upgrade run must leave the migration record clean")

	for _, row := range retiredRows {
		require.Equalf(t, 1, countRows(row.table), "%s must keep exactly the seeded row", row.table)
		require.Equalf(t, row.want, scalar(row.verify), "%s row content must survive the upgrade", row.table)
	}
	for _, row := range coreRows {
		require.Equalf(t, row.want, scalar(row.verify), "%s core row must survive the upgrade", row.table)
	}

	// Core flows keep working after the upgrade: the product writes new
	// knowledge and session rows. Those are the writes that used to be
	// accompanied by retired-table writes, so the retired counts must not move.
	exec(`INSERT INTO knowledges (id, tenant_id, knowledge_base_id, type, title, source) ` +
		`VALUES ('doc-after-upgrade-1', 1, 'kb-legacy-1', 'file', 'after-upgrade.pdf', 'after-upgrade.pdf')`)
	exec(`INSERT INTO sessions (id, tenant_id, title, user_id, knowledge_base_id) ` +
		`VALUES ('session-after-upgrade-1', 1, 'after-upgrade-session', 'user-legacy-1', 'kb-legacy-1')`)
	require.Equal(t, "after-upgrade.pdf", scalar(`SELECT title FROM knowledges WHERE id = 'doc-after-upgrade-1'`))
	require.Equal(t, "after-upgrade-session", scalar(`SELECT title FROM sessions WHERE id = 'session-after-upgrade-1'`))

	for _, row := range retiredRows {
		require.Equalf(t, 1, countRows(row.table),
			"%s must not receive rows from the core flows that ran after the upgrade", row.table)
	}
}

// collectProductionGoFiles returns every non-test .go file below the
// repository's production trees. The walk is rooted at internal/ and cmd/
// directly rather than at the repository root: an earlier revision started at
// the root and rejected it on the first callback, so it scanned zero files and
// still passed. Walking the two trees also keeps migrations/ out of scope by
// construction, and internal/database/testdata is excluded explicitly because
// it holds fixtures. Read and walk errors are returned, never skipped.
func collectProductionGoFiles(repoRoot string) ([]string, error) {
	roots := []string{
		filepath.Join(repoRoot, "internal"),
		filepath.Join(repoRoot, "cmd"),
	}
	excludedPrefixes := []string{
		filepath.Join(repoRoot, "internal", "database", "testdata"),
	}

	var paths []string
	for _, root := range roots {
		info, err := os.Stat(root)
		if err != nil {
			return nil, fmt.Errorf("production tree %s: %w", root, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("production tree %s is not a directory", root)
		}
		err = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				for _, prefix := range excludedPrefixes {
					if p == prefix || strings.HasPrefix(p, prefix+string(filepath.Separator)) {
						return filepath.SkipDir
					}
				}
				return nil
			}
			if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			paths = append(paths, p)
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("walk %s: %w", root, err)
		}
	}
	return paths, nil
}

// scanFilesForORMWrites returns the files whose content contains a GORM write
// chain opened on a retired table.
func scanFilesForORMWrites(t *testing.T, paths []string) []string {
	t.Helper()
	var hits []string
	for _, p := range paths {
		b, err := os.ReadFile(p)
		require.NoErrorf(t, err, "read %s", p)
		if len(retiredTableORMWriters(string(b))) > 0 {
			hits = append(hits, p)
		}
	}
	return hits
}

// scanFilesForMatches returns the paths whose content matches re. A file that
// cannot be read fails the scan: a guard that silently ignores its inputs is
// how the first revision of this test passed while reading nothing.
func scanFilesForMatches(paths []string, re *regexp.Regexp) ([]string, error) {
	var hits []string
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", p, err)
		}
		if re.FindString(string(b)) != "" {
			hits = append(hits, p)
		}
	}
	return hits, nil
}

// writeFixtureFile writes content to root/rel and returns the full path.
func writeFixtureFile(t *testing.T, root, rel, content string) string {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
	require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
	return p
}
