package repository

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckKnowledgeExists_FileHashIsScopedByFileType(t *testing.T) {
	db := setupKnowledgeTestDB(t)
	repo := NewKnowledgeRepository(db)
	ctx := context.Background()
	tenantID := uint64(1)
	kbID := uuid.NewString()
	const fileHash = "same-content-hash"

	require.NoError(t, db.Exec(`
		INSERT INTO knowledges (id, tenant_id, knowledge_base_id, type, title, file_name, file_type, file_hash, parse_status)
		VALUES (?, ?, ?, 'file', 'document.md', 'document.md', 'md', ?, 'completed')
	`, uuid.NewString(), tenantID, kbID, fileHash).Error)

	t.Run("same content with another file type is allowed", func(t *testing.T) {
		exists, knowledge, err := repo.CheckKnowledgeExists(ctx, tenantID, kbID, &types.KnowledgeCheckParams{
			Type:     "file",
			FileHash: fileHash,
			FileType: "txt",
		})

		require.NoError(t, err)
		assert.False(t, exists)
		assert.Nil(t, knowledge)
	})

	t.Run("same content and file type remains a duplicate", func(t *testing.T) {
		exists, knowledge, err := repo.CheckKnowledgeExists(ctx, tenantID, kbID, &types.KnowledgeCheckParams{
			Type:     "file",
			FileHash: fileHash,
			FileType: "md",
		})

		require.NoError(t, err)
		assert.True(t, exists)
		require.NotNil(t, knowledge)
		assert.Equal(t, "md", knowledge.FileType)
	})

	t.Run("file type matching is case-insensitive", func(t *testing.T) {
		exists, knowledge, err := repo.CheckKnowledgeExists(ctx, tenantID, kbID, &types.KnowledgeCheckParams{
			Type:     "file",
			FileHash: fileHash,
			FileType: "MD",
		})

		require.NoError(t, err)
		assert.True(t, exists)
		require.NotNil(t, knowledge)
		assert.Equal(t, "md", knowledge.FileType)
	})
}

func TestCheckKnowledgeExists_ParseStatusMatrix(t *testing.T) {
	// Pins the duplicate-check status policy (issue #3338): failed and
	// deleting rows must not block an upload — a deleting row is on its way
	// out regardless of how the async delete task concludes — while pending,
	// processing and completed rows remain real duplicates.
	for _, tc := range []struct {
		parseStatus string
		want        bool
	}{
		{"failed", false},
		{"deleting", false},
		{"pending", true},
		{"processing", true},
		{"completed", true},
	} {
		t.Run("parse_status="+tc.parseStatus, func(t *testing.T) {
			db := setupKnowledgeTestDB(t)
			repo := NewKnowledgeRepository(db)
			require.NoError(t, db.Exec(`
				INSERT INTO knowledges (id, tenant_id, knowledge_base_id, type, file_name, file_type, file_size, file_hash, parse_status)
				VALUES (?, 1, 'kb-1', 'file', 'doc.md', 'md', 10, 'hash-1', ?)
			`, uuid.NewString(), tc.parseStatus).Error)

			exists, _, err := repo.CheckKnowledgeExists(context.Background(), 1, "kb-1", &types.KnowledgeCheckParams{
				Type: "file", FileHash: "hash-1", FileType: "md",
			})
			require.NoError(t, err)
			require.Equal(t, tc.want, exists)

			// The filename/size fallback path shares the same base filter.
			exists, _, err = repo.CheckKnowledgeExists(context.Background(), 1, "kb-1", &types.KnowledgeCheckParams{
				Type: "file", FileName: "doc.md", FileSize: 10,
			})
			require.NoError(t, err)
			require.Equal(t, tc.want, exists)
		})
	}
}

// TestCountKnowledgeByKnowledgeBaseID_ExcludesDeleting pins the count/list
// alignment (issues #3338/#3345): the document list hides rows mid-deletion,
// so the KB card count must not include them either — otherwise a stranded
// delete shows up as "4 documents, 3 listed".
func TestCountKnowledgeByKnowledgeBaseID_ExcludesDeleting(t *testing.T) {
	db := setupKnowledgeTestDB(t)
	repo := NewKnowledgeRepository(db)
	for _, tc := range []struct{ id, status string }{
		{"k-done", "completed"},
		{"k-failed", "failed"},
		{"k-deleting", "deleting"},
	} {
		require.NoError(t, db.Exec(`
			INSERT INTO knowledges (id, tenant_id, knowledge_base_id, type, file_name, parse_status)
			VALUES (?, 1, 'kb-cnt', 'file', ?, ?)
		`, tc.id, tc.id+".md", tc.status).Error)
	}

	count, err := repo.CountKnowledgeByKnowledgeBaseID(context.Background(), 1, "kb-cnt")
	require.NoError(t, err)
	// failed rows stay counted (they are visible, actionable cards); only
	// the hidden mid-deletion row drops out.
	require.Equal(t, int64(2), count)
}
