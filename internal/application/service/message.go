package service

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// regThinkIndex matches <think>...</think> blocks for stripping from KB index content.
var regThinkIndex = regexp.MustCompile(`(?s)<think>.*?</think>`)

// messageService implements the MessageService interface for managing messaging operations
// It handles creating, retrieving, updating, and deleting messages within sessions.
// It reads the chat history knowledge base configuration from the tenant's ChatHistoryConfig,
// which is managed via the settings UI.
type messageService struct {
	messageRepo    interfaces.MessageRepository    // Repository for message storage operations
	sessionRepo    interfaces.SessionRepository    // Repository for session validation
	tenantService  interfaces.TenantService        // Service for tenant operations (read ChatHistoryConfig)
	kbService      interfaces.KnowledgeBaseService // Service for knowledge base operations (search chat history KB)
	knowService    interfaces.KnowledgeService     // Service for knowledge operations (index/delete passages)
	modelService   interfaces.ModelService         // Service for model operations (rerank model)
	suggestionRepo interfaces.MessageSuggestionRepository
}

// NewMessageService creates a new message service instance with the required repositories
func NewMessageService(messageRepo interfaces.MessageRepository,
	sessionRepo interfaces.SessionRepository,
	tenantService interfaces.TenantService,
	kbService interfaces.KnowledgeBaseService,
	knowService interfaces.KnowledgeService,
	modelService interfaces.ModelService,
	suggestionRepo interfaces.MessageSuggestionRepository,
) interfaces.MessageService {
	return &messageService{
		messageRepo:    messageRepo,
		sessionRepo:    sessionRepo,
		tenantService:  tenantService,
		kbService:      kbService,
		knowService:    knowService,
		modelService:   modelService,
		suggestionRepo: suggestionRepo,
	}
}

// sessionTenantIDForLookup returns the tenant ID to use for session lookup.
// When SessionTenantIDContextKey is set (e.g. pipeline with shared agent), use it so session/message belong to session owner.
func sessionTenantIDForLookup(ctx context.Context) (uint64, bool) {
	if v := ctx.Value(types.SessionTenantIDContextKey); v != nil {
		if tid, ok := v.(uint64); ok && tid != 0 {
			return tid, true
		}
	}
	if v := ctx.Value(types.TenantIDContextKey); v != nil {
		if tid, ok := v.(uint64); ok {
			return tid, true
		}
	}
	return 0, false
}

func sessionUserIDForLookup(ctx context.Context) string {
	if ctx.Value(types.SessionTenantIDContextKey) != nil {
		// Shared-agent pipelines resolve the session owner tenant first; keep that internal lookup tenant-scoped.
		return ""
	}
	return types.SessionOwnerIDFromContext(ctx)
}

// CreateMessage creates a new message within an existing session
func (s *messageService) CreateMessage(ctx context.Context, message *types.Message) (*types.Message, error) {
	logger.Info(ctx, "Start creating message")
	logger.Infof(ctx, "Creating message for session ID: %s", message.SessionID)

	tenantID := types.MustTenantIDFromContext(ctx)
	logger.Infof(ctx, "Checking if session exists, tenant ID: %d, session ID: %s", tenantID, message.SessionID)
	_, err := s.sessionRepo.Get(ctx, tenantID, sessionUserIDForLookup(ctx), message.SessionID)
	if err != nil {
		logger.Errorf(ctx, "Failed to get session: %v", err)
		return nil, err
	}

	logger.Info(ctx, "Session exists, creating message")
	createdMessage, err := s.messageRepo.CreateMessage(ctx, message)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"session_id": message.SessionID,
		})
		return nil, err
	}

	logger.Infof(ctx, "Message created successfully, ID: %s", createdMessage.ID)
	return createdMessage, nil
}

// GetMessage retrieves a specific message by its ID within a session
func (s *messageService) GetMessage(ctx context.Context, sessionID string, messageID string) (*types.Message, error) {
	logger.Info(ctx, "Start getting message")
	logger.Infof(ctx, "Getting message, session ID: %s, message ID: %s", sessionID, messageID)

	tenantID := types.MustTenantIDFromContext(ctx)
	logger.Infof(ctx, "Checking if session exists, tenant ID: %d", tenantID)
	_, err := loadSessionForRead(ctx, s.sessionRepo, tenantID, sessionUserIDForLookup(ctx), sessionID)
	if err != nil {
		logger.Errorf(ctx, "Failed to get session: %v", err)
		return nil, err
	}

	logger.Info(ctx, "Session exists, getting message")
	message, err := s.messageRepo.GetMessage(ctx, sessionID, messageID)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"session_id": sessionID,
			"message_id": messageID,
		})
		return nil, err
	}

	logger.Info(ctx, "Message retrieved successfully")
	return message, nil
}

// GetMessagesBySession retrieves paginated messages for a specific session
func (s *messageService) GetMessagesBySession(ctx context.Context,
	sessionID string, page int, pageSize int,
) ([]*types.Message, error) {
	logger.Info(ctx, "Start getting messages by session")
	logger.Infof(ctx, "Getting messages for session ID: %s, page: %d, pageSize: %d", sessionID, page, pageSize)

	tenantID := types.MustTenantIDFromContext(ctx)
	logger.Infof(ctx, "Checking if session exists, tenant ID: %d", tenantID)
	_, err := loadSessionForRead(ctx, s.sessionRepo, tenantID, sessionUserIDForLookup(ctx), sessionID)
	if err != nil {
		logger.Errorf(ctx, "Failed to get session: %v", err)
		return nil, err
	}

	logger.Info(ctx, "Session exists, getting messages")
	messages, err := s.messageRepo.GetMessagesBySession(ctx, sessionID, page, pageSize)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"session_id": sessionID,
			"page":       page,
			"page_size":  pageSize,
		})
		return nil, err
	}

	logger.Infof(ctx, "Retrieved %d messages successfully", len(messages))
	return messages, nil
}

// GetRecentMessagesBySession retrieves the most recent messages from a session
func (s *messageService) GetRecentMessagesBySession(ctx context.Context,
	sessionID string, limit int,
) ([]*types.Message, error) {
	logger.Info(ctx, "Start getting recent messages by session")
	logger.Infof(ctx, "Getting recent messages for session ID: %s, limit: %d", sessionID, limit)

	tenantID, ok := sessionTenantIDForLookup(ctx)
	if !ok {
		logger.Error(ctx, "Workspace ID not found in context for session lookup")
		return nil, errors.New("workspace ID not found in context")
	}
	logger.Infof(ctx, "Checking if session exists, tenant ID: %d", tenantID)
	_, err := loadSessionForRead(ctx, s.sessionRepo, tenantID, sessionUserIDForLookup(ctx), sessionID)
	if err != nil {
		logger.Errorf(ctx, "Failed to get session: %v", err)
		return nil, err
	}

	logger.Info(ctx, "Session exists, getting recent messages")
	messages, err := s.messageRepo.GetRecentMessagesBySession(ctx, sessionID, limit)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"session_id": sessionID,
			"limit":      limit,
		})
		return nil, err
	}

	logger.Infof(ctx, "Retrieved %d recent messages successfully", len(messages))
	return messages, nil
}

// GetMessagesBySessionBeforeTime retrieves messages sent before a specific time
func (s *messageService) GetMessagesBySessionBeforeTime(ctx context.Context,
	sessionID string, beforeTime time.Time, limit int,
) ([]*types.Message, error) {
	logger.Info(ctx, "Start getting messages before time")
	logger.Infof(ctx, "Getting messages before %v for session ID: %s, limit: %d", beforeTime, sessionID, limit)

	tenantID, ok := sessionTenantIDForLookup(ctx)
	if !ok {
		logger.Error(ctx, "Workspace ID not found in context for session lookup")
		return nil, errors.New("workspace ID not found in context")
	}
	logger.Infof(ctx, "Checking if session exists, tenant ID: %d", tenantID)
	_, err := loadSessionForRead(ctx, s.sessionRepo, tenantID, sessionUserIDForLookup(ctx), sessionID)
	if err != nil {
		logger.Errorf(ctx, "Failed to get session: %v", err)
		return nil, err
	}

	logger.Info(ctx, "Session exists, getting messages before time")
	messages, err := s.messageRepo.GetMessagesBySessionBeforeTime(ctx, sessionID, beforeTime, limit)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"session_id":  sessionID,
			"before_time": beforeTime,
			"limit":       limit,
		})
		return nil, err
	}

	logger.Infof(ctx, "Retrieved %d messages before time successfully", len(messages))
	return messages, nil
}

// UpdateMessage updates an existing message's content or metadata
func (s *messageService) UpdateMessage(ctx context.Context, message *types.Message) error {
	logger.Info(ctx, "Start updating message")
	logger.Infof(ctx, "Updating message, ID: %s, session ID: %s", message.ID, message.SessionID)

	tenantID := types.MustTenantIDFromContext(ctx)
	logger.Infof(ctx, "Checking if session exists, tenant ID: %d", tenantID)
	_, err := s.sessionRepo.Get(ctx, tenantID, sessionUserIDForLookup(ctx), message.SessionID)
	if err != nil {
		logger.Errorf(ctx, "Failed to get session: %v", err)
		return err
	}

	logger.Info(ctx, "Session exists, updating message")
	err = s.messageRepo.UpdateMessage(ctx, message)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"session_id": message.SessionID,
			"message_id": message.ID,
		})
		return err
	}

	logger.Info(ctx, "Message updated successfully")
	return nil
}

// UpdateMessageImages updates only the images JSONB column for a message.
func (s *messageService) UpdateMessageImages(ctx context.Context, sessionID, messageID string, images types.MessageImages) error {
	return s.messageRepo.UpdateMessageImages(ctx, sessionID, messageID, images)
}

// UpdateMessageRenderedContent updates the rendered_content column for a user message.
func (s *messageService) UpdateMessageRenderedContent(ctx context.Context, sessionID, messageID string, renderedContent string) error {
	return s.messageRepo.UpdateMessageRenderedContent(ctx, sessionID, messageID, renderedContent)
}

// DeleteMessage removes a message from a session, also cleaning up its Knowledge entry in the chat history KB.
func (s *messageService) DeleteMessage(ctx context.Context, sessionID string, messageID string) error {
	logger.Info(ctx, "Start deleting message")
	logger.Infof(ctx, "Deleting message, session ID: %s, message ID: %s", sessionID, messageID)

	tenantID := types.MustTenantIDFromContext(ctx)
	logger.Infof(ctx, "Checking if session exists, tenant ID: %d", tenantID)
	_, err := s.sessionRepo.Get(ctx, tenantID, sessionUserIDForLookup(ctx), sessionID)
	if err != nil {
		logger.Errorf(ctx, "Failed to get session: %v", err)
		return err
	}

	// Get the message first to check if it has an associated Knowledge entry
	msg, err := s.messageRepo.GetMessage(ctx, sessionID, messageID)
	if err != nil {
		logger.Errorf(ctx, "Failed to get message for deletion: %v", err)
		return err
	}

	// Delete the message from the repository
	logger.Info(ctx, "Session exists, deleting message")
	err = s.messageRepo.DeleteMessage(ctx, sessionID, messageID)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"session_id": sessionID,
			"message_id": messageID,
		})
		return err
	}
	if s.suggestionRepo != nil {
		if err := s.suggestionRepo.DeleteByMessageID(ctx, tenantID, sessionID, messageID); err != nil {
			logger.Warnf(ctx, "Failed to delete suggestions for message %s: %v", messageID, err)
		}
	}

	// Async cleanup: delete the associated Knowledge entry from the chat history KB.
	// Use WithoutCancel so the goroutine survives after the HTTP request context is done.
	if msg != nil && msg.KnowledgeID != "" {
		bgCtx := context.WithoutCancel(ctx)
		go s.DeleteMessageKnowledge(bgCtx, msg.KnowledgeID)
	}

	logger.Info(ctx, "Message deleted successfully")
	return nil
}

// ClearSessionMessages deletes all messages in a session, along with their chat history KB entries.
func (s *messageService) ClearSessionMessages(ctx context.Context, sessionID string) error {
	logger.Infof(ctx, "Start clearing all messages for session: %s", sessionID)

	tenantID := types.MustTenantIDFromContext(ctx)
	if _, err := s.sessionRepo.Get(ctx, tenantID, sessionUserIDForLookup(ctx), sessionID); err != nil {
		logger.Errorf(ctx, "Failed to get session: %v", err)
		return err
	}

	// Async cleanup: delete associated Knowledge entries from the chat history KB
	bgCtx := context.WithoutCancel(ctx)
	go s.DeleteSessionKnowledge(bgCtx, sessionID)

	if err := s.messageRepo.DeleteMessagesBySessionID(ctx, sessionID); err != nil {
		logger.Errorf(ctx, "Failed to delete messages for session %s: %v", sessionID, err)
		return err
	}
	if s.suggestionRepo != nil {
		if err := s.suggestionRepo.DeleteBySessionID(ctx, tenantID, sessionID); err != nil {
			logger.Warnf(ctx, "Failed to delete suggestions for session %s: %v", sessionID, err)
		}
	}

	logger.Infof(ctx, "All messages cleared for session: %s", sessionID)
	return nil
}

// DeleteMessageKnowledge deletes the Knowledge entry associated with a message from the chat history KB.
func (s *messageService) DeleteMessageKnowledge(ctx context.Context, knowledgeID string) {
	if knowledgeID == "" {
		return
	}
	logger.Infof(ctx, "Deleting chat history knowledge entry: %s", knowledgeID)
	if err := deleteReferencedKnowledge(ctx, s.knowService, "", []string{knowledgeID}); err != nil {
		logger.Warnf(ctx, "Failed to delete chat history knowledge %s: %v", knowledgeID, err)
	}
}

// DeleteSessionKnowledge deletes all Knowledge entries for messages in a session from the chat history KB.
func (s *messageService) DeleteSessionKnowledge(ctx context.Context, sessionID string) {
	logger.Infof(ctx, "Deleting all chat history knowledge entries for session: %s", sessionID)

	knowledgeIDs, err := s.messageRepo.GetKnowledgeIDsBySessionID(ctx, sessionID)
	if err != nil {
		logger.Warnf(ctx, "Failed to get knowledge IDs for session %s: %v", sessionID, err)
		return
	}

	if len(knowledgeIDs) == 0 {
		return
	}

	logger.Infof(ctx, "Deleting %d chat history knowledge entries for session %s", len(knowledgeIDs), sessionID)
	if err := deleteReferencedKnowledge(ctx, s.knowService, "", knowledgeIDs); err != nil {
		logger.Warnf(ctx, "Failed to batch delete chat history knowledge for session %s: %v", sessionID, err)
	}
}

// artifactLibraryMaxPageSize caps one library page; the page renders a card
// per row, so larger pages only cost bandwidth.
const artifactLibraryMaxPageSize = 100

// normalizeArtifactFileTypes lower-cases extensions and adds the leading dot
// the collector stores ("pdf" and ".PDF" both become ".pdf").
func normalizeArtifactFileTypes(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, raw := range in {
		ext := strings.ToLower(strings.TrimSpace(raw))
		if ext == "" {
			continue
		}
		if !strings.HasPrefix(ext, ".") {
			ext = "." + ext
		}
		if !seen[ext] {
			seen[ext] = true
			out = append(out, ext)
		}
	}
	return out
}

// rerankResults applies rerank model to search results if configured.
// Returns reranked + filtered results, or original results if rerank is unavailable.
func (s *messageService) rerankResults(ctx context.Context, rc *types.RetrievalConfig, query string, results []*types.SearchResult) []*types.SearchResult {
	if rc == nil || rc.RerankModelID == "" || len(results) == 0 {
		return results
	}

	reranker, err := s.modelService.GetRerankModel(ctx, rc.RerankModelID)
	if err != nil {
		logger.Warnf(ctx, "Failed to get rerank model %s, skipping rerank: %v", rc.RerankModelID, err)
		return results
	}

	// Build documents for rerank
	documents := make([]string, len(results))
	for i, r := range results {
		documents[i] = r.Content
	}

	rankResults, err := reranker.Rerank(ctx, query, documents)
	if err != nil {
		logger.Warnf(ctx, "Rerank call failed, skipping: %v", err)
		return results
	}

	// Filter by threshold and topK, rebuild results with rerank scores
	threshold := rc.GetEffectiveRerankThreshold()
	topK := rc.GetEffectiveRerankTopK()

	var reranked []*types.SearchResult
	for _, rr := range rankResults {
		if rr.Index >= len(results) {
			continue
		}
		if rr.RelevanceScore < threshold {
			continue
		}
		item := *results[rr.Index]
		item.Score = rr.RelevanceScore
		reranked = append(reranked, &item)
		if len(reranked) >= topK {
			break
		}
	}

	logger.Infof(ctx, "Rerank: %d -> %d results (threshold=%.2f, topK=%d)", len(results), len(reranked), threshold, topK)
	return reranked
}
