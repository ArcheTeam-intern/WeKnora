package handler

import (
	stderrors "errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/storageurl"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	secutils "github.com/Tencent/WeKnora/internal/utils"
)

// MessageHandler handles HTTP requests related to messages within chat sessions
// It provides endpoints for loading and managing message history
type MessageHandler struct {
	MessageService interfaces.MessageService // Service that implements message business logic
	// FileService and StorageResolver back the optional `resource_urls=public`
	// mode, which returns loadable HTTP URLs instead of internal
	// `resource://` handles. Both may be nil, in which case only the default
	// handle mode is available.
	FileService     interfaces.FileService
	StorageResolver interfaces.StorageBackendResolver
}

// NewMessageHandler creates a new message handler instance with the required service
// Parameters:
//   - messageService: Service that implements message business logic
//   - fileService: Storage access used to sign public resource URLs
//   - storageResolver: Resolves per-tenant storage backends for those URLs
//
// Returns a pointer to a new MessageHandler
func NewMessageHandler(
	messageService interfaces.MessageService,
	fileService interfaces.FileService,
	storageResolver interfaces.StorageBackendResolver,
) *MessageHandler {
	return &MessageHandler{
		MessageService:  messageService,
		FileService:     fileService,
		StorageResolver: storageResolver,
	}
}

// resolveResourceRewriter builds the storage-reference rewriter for one response
// from the request's `resource_urls` parameter, falling back to the deployment
// default. The returned error is already an AppError the caller can hand to
// c.Error: a rejected scope is a 403, a typo in the parameter is a 400.
func (h *MessageHandler) resolveResourceRewriter(c *gin.Context) (*storageurl.Rewriter, error) {
	ctx := c.Request.Context()
	mode, err := storageurl.ResolveMode(ctx, c.Query(storageurl.QueryParam))
	if err != nil {
		if stderrors.Is(err, storageurl.ErrPublicModeForbidden) {
			return nil, errors.NewForbiddenError(err.Error())
		}
		return nil, errors.NewBadRequestError(err.Error())
	}
	return storageurl.NewRequestRewriter(ctx, mode, h.FileService, h.StorageResolver), nil
}

// LoadMessages godoc
// @Summary      加载消息历史
// @Description  加载会话的消息历史，支持分页和时间筛选
// @Tags         消息
// @Accept       json
// @Produce      json
// @Param        session_id     path      string  true   "会话ID"
// @Param        limit          query     int     false  "返回数量"  default(20)
// @Param        before_time    query     string  false  "在此时间之前的消息（RFC3339Nano格式）"
// @Param        resource_urls  query     string  false  "文件引用形式，public 返回可加载直链"  Enums(handle, public)  default(handle)
// @Success      200            {object}  map[string]interface{}  "消息列表"
// @Failure      400          {object}  errors.AppError         "请求参数错误"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /messages/{session_id}/load [get]
func (h *MessageHandler) LoadMessages(c *gin.Context) {
	ctx := c.Request.Context()

	logger.Info(ctx, "Start loading messages")

	// Get path parameters and query parameters
	sessionID := secutils.SanitizeForLog(c.Param("session_id"))
	limit := secutils.SanitizeForLog(c.DefaultQuery("limit", "20"))
	beforeTimeStr := secutils.SanitizeForLog(c.DefaultQuery("before_time", ""))

	logger.Infof(ctx, "Loading messages params, session ID: %s, limit: %s, before time: %s",
		sessionID, limit, beforeTimeStr)

	rewriter, err := h.resolveResourceRewriter(c)
	if err != nil {
		logger.Warnf(ctx, "Rejected resource URL mode: %v", err)
		_ = c.Error(err)
		return
	}

	// Parse limit parameter with fallback to default
	limitInt, convErr := strconv.Atoi(limit)
	if convErr != nil {
		logger.Warnf(ctx, "Invalid limit value, using default value 20, input: %s", limit)
		limitInt = 20
	}

	// If no beforeTime is provided, retrieve the most recent messages
	if beforeTimeStr == "" {
		logger.Infof(ctx, "Getting recent messages for session, session ID: %s, limit: %d", sessionID, limitInt)
		messages, err := h.MessageService.GetRecentMessagesBySession(ctx, sessionID, limitInt)
		if err != nil {
			if stderrors.Is(err, errors.ErrSessionNotFound) {
				// PR #1309 plumbed user-scope into the message service's
				// session existence check; non-owner / wrong-tenant lookups
				// surface as ErrSessionNotFound. Map to 404 so clients can
				// tell "wrong URL" from a real 5xx.
				logger.Warnf(ctx, "Session not found, ID: %s", sessionID)
				c.Error(errors.NewNotFoundError(err.Error()))
				return
			}
			logger.ErrorWithFields(ctx, err, nil)
			c.Error(errors.NewInternalServerError(err.Error()))
			return
		}

		logger.Infof(
			ctx,
			"Successfully retrieved recent messages, session ID: %s, message count: %d",
			sessionID, len(messages),
		)
		c.JSON(http.StatusOK, gin.H{
			"success": true,
			"data":    rewriter.RewriteMessagesResponse(ctx, messages),
		})
		return
	}

	// If beforeTime is provided, parse the timestamp (RFC3339Nano or RFC3339).
	beforeTime, err := parseMessageBeforeTime(beforeTimeStr)
	if err != nil {
		logger.Errorf(
			ctx,
			"Invalid time format, please use RFC3339/RFC3339Nano format, err: %v, beforeTimeStr: %s",
			err, beforeTimeStr,
		)
		c.Error(errors.NewBadRequestError("Invalid time format, please use RFC3339 or RFC3339Nano format"))
		return
	}

	// Retrieve messages before the specified timestamp
	logger.Infof(ctx, "Getting messages before specific time, session ID: %s, before time: %s, limit: %d",
		sessionID, beforeTime.Format(time.RFC3339Nano), limitInt)
	messages, err := h.MessageService.GetMessagesBySessionBeforeTime(ctx, sessionID, beforeTime, limitInt)
	if err != nil {
		if stderrors.Is(err, errors.ErrSessionNotFound) {
			// See note on the GetRecentMessagesBySession path above.
			logger.Warnf(ctx, "Session not found, ID: %s", sessionID)
			c.Error(errors.NewNotFoundError(err.Error()))
			return
		}
		logger.ErrorWithFields(ctx, err, nil)
		c.Error(errors.NewInternalServerError(err.Error()))
		return
	}

	logger.Infof(
		ctx,
		"Successfully retrieved messages before time, session ID: %s, message count: %d",
		sessionID, len(messages),
	)
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    rewriter.RewriteMessagesResponse(ctx, messages),
	})
}

// DeleteMessage godoc
// @Summary      删除消息
// @Description  从会话中删除指定消息
// @Tags         消息
// @Accept       json
// @Produce      json
// @Param        session_id  path      string  true  "会话ID"
// @Param        id          path      string  true  "消息ID"
// @Success      200         {object}  map[string]interface{}  "删除成功"
// @Failure      500         {object}  errors.AppError         "服务器错误"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /messages/{session_id}/{id} [delete]
func (h *MessageHandler) DeleteMessage(c *gin.Context) {
	ctx := c.Request.Context()

	logger.Info(ctx, "Start deleting message")

	// Get path parameters for session and message identification
	sessionID := secutils.SanitizeForLog(c.Param("session_id"))
	messageID := secutils.SanitizeForLog(c.Param("id"))

	logger.Infof(ctx, "Deleting message, session ID: %s, message ID: %s", sessionID, messageID)

	// Delete the message using the message service
	if err := h.MessageService.DeleteMessage(ctx, sessionID, messageID); err != nil {
		if stderrors.Is(err, errors.ErrSessionNotFound) {
			// See note on LoadMessages above — message-service operations
			// surface ErrSessionNotFound when the caller can't see the
			// owning session (post-#1309 user scope).
			logger.Warnf(ctx, "Session not found, ID: %s", sessionID)
			c.Error(errors.NewNotFoundError(err.Error()))
			return
		}
		if stderrors.Is(err, gorm.ErrRecordNotFound) {
			// The message_id doesn't exist under this session — a client error,
			// not a server fault. 404 so callers read resource.not_found (a
			// permanent condition, not retryable) instead of a 5xx. Mirrors the
			// ContinueStream / kb / doc / chunk not-found handling.
			logger.Warnf(ctx, "Message not found, session ID: %s, message ID: %s", sessionID, messageID)
			c.Error(errors.NewNotFoundError(err.Error()))
			return
		}
		logger.ErrorWithFields(ctx, err, nil)
		c.Error(errors.NewInternalServerError(err.Error()))
		return
	}

	logger.Infof(ctx, "Message deleted successfully, session ID: %s, message ID: %s", sessionID, messageID)
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "Message deleted successfully",
	})
}

// Frontend cursors may be RFC3339 (no fractional seconds) or RFC3339Nano.
func parseMessageBeforeTime(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, stderrors.New("empty before_time")
	}
	layouts := []string{time.RFC3339Nano, time.RFC3339}
	var lastErr error
	for _, layout := range layouts {
		if t, err := time.Parse(layout, raw); err == nil {
			return t, nil
		} else {
			lastErr = err
		}
	}
	return time.Time{}, lastErr
}
