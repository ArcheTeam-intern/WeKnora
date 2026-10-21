package service

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/require"
)

type wikiVersionGuardRepo struct {
	interfaces.WikiPageRepository
	mutated bool
}

func (*wikiVersionGuardRepo) GetBySlug(context.Context, string, string) (*types.WikiPage, error) {
	return &types.WikiPage{ID: "page", KnowledgeBaseID: "kb", Slug: "concept/race", Version: 3, Content: "concurrent content"}, nil
}

func (r *wikiVersionGuardRepo) UpdateWithRevision(context.Context, *types.WikiPage, *types.WikiPageRevision) error {
	r.mutated = true
	return nil
}

// The caller read version 2, then another writer committed version 3 before
// UpdatePage reloaded it. The service must refuse before changing any state.
func TestUpdateWikiPageRejectsCallerVersionBeforeMutation(t *testing.T) {
	repo := &wikiVersionGuardRepo{}
	svc := NewWikiPageService(repo, nil, nil, nil, nil, nil)
	_, err := svc.UpdatePage(context.Background(), &types.WikiPage{KnowledgeBaseID: "kb", Slug: "concept/race", Version: 2, Content: "stale overwrite"})
	require.ErrorIs(t, err, repository.ErrWikiPageConflict)
	require.False(t, repo.mutated)
}
