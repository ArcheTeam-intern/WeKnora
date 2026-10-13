package sqlite

import (
	"strings"
	"testing"
)

// TestFTS5ModuleIsPresentInThisBuild fails (rather than skips) when the driver
// was compiled without the fts5 module.
//
// The other keyword-index tests skip in that situation, which is the right
// behaviour for the upstream test suite but not for the WeKnora-Agent
// prototype: the whole point of building with `-tags sqlite_fts5` is that the
// keyword (BM25) index exists, and a build that silently lost the tag would
// otherwise report a fully green run. The prototype's weknora-check runner
// executes this package with that tag, so this test is its FTS5 assertion.
func TestFTS5ModuleIsPresentInThisBuild(t *testing.T) {
	repository := newSQLiteRetrieverTestRepository(t)
	if repository.ftsUnavailable {
		t.Fatal("sqlite driver was built without the fts5 module: " +
			"build the prototype with -tags sqlite_fts5 (see deploy/Dockerfile.prototype-app)")
	}
	if !repository.db.Migrator().HasTable("lite_embeddings_fts") {
		t.Fatal("the lite_embeddings_fts virtual table was not created")
	}
	// An FTS5 virtual table answers a MATCH query; a plain table would not.
	var count int64
	err := repository.db.Raw(
		`SELECT count(*) FROM lite_embeddings_fts WHERE lite_embeddings_fts MATCH ?`, "prototype",
	).Scan(&count).Error
	if err != nil {
		if strings.Contains(err.Error(), "no such module: fts5") {
			t.Fatalf("fts5 module missing at query time: %v", err)
		}
		t.Fatalf("FTS5 MATCH query failed: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected an empty fresh index, got %d rows", count)
	}
}
