package interfaces

import (
	"context"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
)

// MessageService defines the message service interface
type MessageService interface {
	// CreateMessage creates a message
	CreateMessage(ctx context.Context, message *types.Message) (*types.Message, error)

	// GetMessage gets a message
	GetMessage(ctx context.Context, sessionID string, id string) (*types.Message, error)

	// GetMessagesBySession gets all messages of a session
	GetMessagesBySession(ctx context.Context, sessionID string, page int, pageSize int) ([]*types.Message, error)

	// GetRecentMessagesBySession gets recent messages of a session
	GetRecentMessagesBySession(ctx context.Context, sessionID string, limit int) ([]*types.Message, error)

	// GetMessagesBySessionBeforeTime gets messages before a specific time of a session
	GetMessagesBySessionBeforeTime(
		ctx context.Context, sessionID string, beforeTime time.Time, limit int,
	) ([]*types.Message, error)

	// UpdateMessage updates a message
	UpdateMessage(ctx context.Context, message *types.Message) error

	// UpdateMessageImages updates only the images JSONB column for a message.
	UpdateMessageImages(ctx context.Context, sessionID, messageID string, images types.MessageImages) error

	// UpdateMessageRenderedContent updates the rendered_content column for a user message.
	UpdateMessageRenderedContent(ctx context.Context, sessionID, messageID string, renderedContent string) error

	// DeleteMessage deletes a message
	DeleteMessage(ctx context.Context, sessionID string, id string) error

	// ClearSessionMessages deletes all messages in a session, along with their chat history KB entries
	ClearSessionMessages(ctx context.Context, sessionID string) error

	// DeleteMessageKnowledge deletes the Knowledge entry associated with a message from the chat history KB.
	DeleteMessageKnowledge(ctx context.Context, knowledgeID string)

	// DeleteSessionKnowledge deletes all Knowledge entries for messages in a session from the chat history KB.
	DeleteSessionKnowledge(ctx context.Context, sessionID string)
}

// MessageRepository defines the message repository interface
type MessageRepository interface {
	// CreateMessage creates a message
	CreateMessage(ctx context.Context, message *types.Message) (*types.Message, error)
	// GetMessage gets a message
	GetMessage(ctx context.Context, sessionID string, id string) (*types.Message, error)
	// GetMessagesBySession gets all messages of a session
	GetMessagesBySession(ctx context.Context, sessionID string, page int, pageSize int) ([]*types.Message, error)
	// GetRecentMessagesBySession gets recent messages of a session
	GetRecentMessagesBySession(ctx context.Context, sessionID string, limit int) ([]*types.Message, error)
	// GetMessagesBySessionBeforeTime gets messages before a specific time of a session
	GetMessagesBySessionBeforeTime(
		ctx context.Context, sessionID string, beforeTime time.Time, limit int,
	) ([]*types.Message, error)
	// ListMessagesBySessionBeforeCursor pages a session backwards: up to limit
	// messages sorting strictly before (before, beforeID), newest first. A zero
	// cursor starts from the newest message.
	ListMessagesBySessionBeforeCursor(
		ctx context.Context, sessionID string, before time.Time, beforeID string, limit int,
	) ([]*types.Message, error)
	// ListMessagesBySessionUpTo returns every message sorting strictly before
	// the (boundary, boundaryID) composite cursor, oldest first. Used by
	// session fork to copy the history preceding a fork point.
	ListMessagesBySessionUpTo(
		ctx context.Context, sessionID string, boundary time.Time, boundaryID string,
	) ([]*types.Message, error)
	// UpdateMessage updates a message
	UpdateMessage(ctx context.Context, message *types.Message) error
	// UpdateMessageImages updates only the images JSONB column for a message
	UpdateMessageImages(ctx context.Context, sessionID, messageID string, images types.MessageImages) error
	// UpdateMessageRenderedContent updates the rendered_content column for a user message
	UpdateMessageRenderedContent(ctx context.Context, sessionID, messageID string, renderedContent string) error
	// UpdateMessageContextCheckpoint writes only the context_checkpoint column
	// of an assistant message.
	UpdateMessageContextCheckpoint(
		ctx context.Context, sessionID, messageID string, checkpoint *types.ContextCheckpoint,
	) error
	// GetLatestContextCheckpoint returns the newest assistant message in the
	// session that carries a context checkpoint, projected to the columns
	// history loading needs, or nil when there is none.
	GetLatestContextCheckpoint(ctx context.Context, sessionID string) (*types.Message, error)
	// DeleteMessage deletes a message
	DeleteMessage(ctx context.Context, sessionID string, id string) error
	// DeleteMessagesFrom deletes every message at or after the
	// (boundary, boundaryID) composite cursor and returns the deleted rows,
	// oldest first. inclusive keeps or drops the boundary message itself,
	// which is what separates a user rewind point from an assistant one.
	DeleteMessagesFrom(
		ctx context.Context, sessionID string, boundary time.Time, boundaryID string, inclusive bool,
	) ([]*types.Message, error)
	// DeleteMessagesBySessionID deletes all messages belonging to a session
	DeleteMessagesBySessionID(ctx context.Context, sessionID string) error
	// GetFirstMessageOfUser gets the first message of a user
	GetFirstMessageOfUser(ctx context.Context, sessionID string) (*types.Message, error)
	// SearchMessagesByKeyword searches messages by keyword across sessions for a tenant
	// OwnedSessionIDs narrows a set of session ids to the ones this person owns.
	OwnedSessionIDs(ctx context.Context, tenantID uint64, ownerID string, sessionIDs []string) (map[string]bool, error)
	SearchMessagesByKeyword(ctx context.Context, tenantID uint64, ownerID, keyword string, sessionIDs []string, limit int) ([]*types.MessageWithSession, error)
	// GetMessagesByKnowledgeIDs retrieves messages by their associated Knowledge IDs
	GetMessagesByKnowledgeIDs(ctx context.Context, knowledgeIDs []string) ([]*types.MessageWithSession, error)
	// GetMessagesByRequestIDs retrieves messages by request ID inside one session
	// (used to fetch Q&A pair partners). Empty sessionID returns no rows.
	GetMessagesByRequestIDs(
		ctx context.Context, sessionID string, requestIDs []string,
	) ([]*types.MessageWithSession, error)
	// GetKnowledgeIDsBySessionID retrieves all knowledge IDs for messages in a session
	GetKnowledgeIDsBySessionID(ctx context.Context, sessionID string) ([]string, error)
	// UpdateMessageKnowledgeID updates the knowledge_id field for a message
	UpdateMessageKnowledgeID(ctx context.Context, messageID string, knowledgeID string) error
	// GetSessionAttachments returns every user-uploaded attachment recorded in
	// the session. Implementations should project only the attachments column.
	GetSessionAttachments(ctx context.Context, sessionID string) (types.MessageAttachments, error)
}
