package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/google/uuid"
	"github.com/mark3labs/mcp-go/mcp"
)

const (
	askTimeout             = 4 * time.Minute
	askCompleteWaitTimeout = 15 * time.Second
	askSessionTitleMaxLen  = 60
	askChannel             = "mcp"
)

func askTool() mcp.Tool {
	return mcp.NewTool(types.MCPEndpointToolAsk,
		mcp.WithDescription("Ask the workspace a question and get a synthesized answer with citations. "+
			"WeKnora retrieves from the knowledge bases in scope and runs the agent configured on this "+
			"endpoint. Pass the returned session_id on follow-up questions to keep the conversation going. "+
			"This can take up to a few minutes for agentic runs; prefer search_knowledge when you only need "+
			"raw passages."),
		mcp.WithString("question", mcp.Required(), mcp.Description("The question to answer")),
		mcp.WithString("session_id",
			mcp.Description("Session id returned by a previous ask call, to continue that conversation")),
		mcp.WithString("mode",
			mcp.Description("Optional native QA mode: quick-answer = RAG with citations; "+
				"smart-reasoning = multi-step reasoning. Omit to use the agent configured on this endpoint."),
			mcp.WithStringEnumItems([]string{"quick-answer", "smart-reasoning"})),
		mcp.WithArray("knowledge_base_ids", mcp.WithStringItems(),
			mcp.Description("Optional knowledge base ids or names to retrieve from; defaults to the "+
				"endpoint scope or the agent's own configuration")),
		// Not read-only: it creates a session and messages, and the configured
		// agent may run its own tools. Clients must not auto-approve it.
		mcp.WithReadOnlyHintAnnotation(false),
	)
}

type askReference struct {
	KnowledgeID    string     `json:"knowledge_id"`
	KnowledgeTitle string     `json:"knowledge_title,omitempty"`
	ChunkID        string     `json:"chunk_id"`
	Score          float64    `json:"score,omitempty"`
	Excerpt        string     `json:"excerpt"`
	Images         []askImage `json:"images,omitempty"`
}

// Keep image references separate from the truncated excerpt. OriginalURL is
// parser provenance, not a serving URL, and must not be exposed as a fallback.
type askImage struct {
	URL     string `json:"url"`
	Caption string `json:"caption,omitempty"`
	OCRText string `json:"ocr_text,omitempty"`
}

func (s *Server) handleAsk(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ep, err := endpointFromContext(ctx)
	if err != nil {
		return mcp.NewToolResultError("unauthorized"), nil
	}
	question, err := req.RequireString("question")
	if err != nil || strings.TrimSpace(question) == "" {
		return mcp.NewToolResultError("question is required"), nil
	}
	question = strings.TrimSpace(question)

	mode, _, err := resolveAskMode(req.GetString("mode", ""))
	if err != nil {
		return errorResult(&ToolError{Code: CodeInvalidArgument, Message: err.Error()}), nil
	}
	agent, err := s.resolveAskAgent(ctx, ep, mode)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	kbIDs, err := s.resolveAskKnowledgeBases(ctx, ep, agent, req.GetStringSlice("knowledge_base_ids", nil))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	session, err := s.resolveAskSession(ctx, ep, req.GetString("session_id", ""), question)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	answer, refs, err := s.runQA(ctx, session, agent, question, kbIDs)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("failed to answer", err), nil
	}
	structured := map[string]any{
		"answer":             answer,
		"session_id":         session.ID,
		"agent_id":           agent.ID,
		"mode":               resolvedModeLabel(mode),
		"knowledge_base_ids": kbIDs,
		"references":         refs,
	}
	var text string
	if mode != "" {
		text = fmt.Sprintf("QA mode: %s\n\n", mode)
	}
	if len(refs) > 0 {
		var b strings.Builder
		b.WriteString(answer)
		b.WriteString("\n\nSources:\n")
		for i, r := range refs {
			title := r.KnowledgeTitle
			if title == "" {
				title = r.KnowledgeID
			}
			fmt.Fprintf(&b, "[%d] %s (document %s)\n", i+1, title, r.KnowledgeID)
			for _, img := range r.Images {
				// Preserve image references for text-only MCP hosts as well.
				// strconv.Quote, unlike json.Marshal, keeps & < > literal.
				fmt.Fprintf(&b, "  Image: %s\n", strconv.Quote(img.URL))
			}
		}
		fmt.Fprintf(&b, "\nsession_id: %s", session.ID)
		text = b.String()
	} else {
		text = fmt.Sprintf("%s\n\nsession_id: %s", answer, session.ID)
	}
	return mcp.NewToolResultStructured(structured, text), nil
}

// askModeAgents maps the client-facing native QA modes onto the builtin agents
// that implement them. Only these two names are reachable through `ask`: the
// endpoint's configured agent remains the default when no mode is passed, and
// a client still cannot name an arbitrary (or internal write-capable) agent id.
var askModeAgents = map[string]string{
	"quick-answer":    types.BuiltinQuickAnswerID,
	"smart-reasoning": types.BuiltinSmartReasoningID,
}

// resolveAskMode validates the optional native mode and maps it to the builtin
// agent that implements it. An empty mode keeps the endpoint default.
func resolveAskMode(mode string) (string, string, error) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		return "", "", nil
	}
	if agentID, ok := askModeAgents[mode]; ok {
		return mode, agentID, nil
	}
	names := make([]string, 0, len(askModeAgents))
	for name := range askModeAgents {
		names = append(names, name)
	}
	sort.Strings(names)
	return "", "", fmt.Errorf("mode %q is not one of the native QA modes: %s", mode, strings.Join(names, ", "))
}

// resolvedModeLabel reports the mode the server actually applied, for the
// response payload the acceptance matrix records.
func resolvedModeLabel(requested string) string {
	if requested == "" {
		return "endpoint-default"
	}
	return requested
}

// resolveAskAgent picks the agent configured on the endpoint. Callers cannot
// name an agent: letting a client choose any tenant agent (or an internal
// builtin such as the wiki fixer or skill installer, which carry write and
// shell tools) would bypass the endpoint's tool allowlist. A requested native
// mode pins the builtin agent that implements it.
func (s *Server) resolveAskAgent(ctx context.Context, ep *types.MCPEndpoint, mode string) (*types.CustomAgent, error) {
	_, modeAgentID, modeErr := resolveAskMode(mode)
	if modeErr != nil {
		return nil, modeErr
	}
	agentID := modeAgentID
	if agentID == "" {
		agentID = strings.TrimSpace(ep.DefaultAgentID)
		if agentID == "" {
			agentID = types.BuiltinQuickAnswerID
		}
	}
	agent, err := s.agentService.GetAgentByID(ctx, agentID)
	if err != nil || agent == nil {
		return nil, fmt.Errorf("the agent configured on this endpoint (%q) is not available", agentID)
	}
	if !types.MCPEndpointAgentAllowed(agent, ep.TenantID) {
		return nil, fmt.Errorf("the agent configured on this endpoint (%q) is not available", agentID)
	}
	return agent, nil
}

// resolveAskKnowledgeBases picks the retrieval scope for one ask call:
// explicit selectors win; otherwise a restricted endpoint pins its own list;
// otherwise the agent's configuration decides, except that an agent with no
// knowledge bases of its own falls back to everything the endpoint can see
// so the call does not fail with "no search targets".
//
// Every successful return is a RESOLVED set, never nil: an agent-owned
// default is narrowed to the caller-authorized canonical ids the same way
// native QA would read them. The core ask binding pins what this returns, so
// an omitted selector cannot silently move a continued conversation onto
// libraries the previous turn did not read (review 2026-10-19 P2).
func (s *Server) resolveAskKnowledgeBases(
	ctx context.Context, ep *types.MCPEndpoint, agent *types.CustomAgent, requested []string,
) ([]string, error) {
	hasRequest := false
	for _, r := range requested {
		if strings.TrimSpace(r) != "" {
			hasRequest = true
			break
		}
	}
	if hasRequest {
		kbs, err := s.selectKnowledgeBases(ctx, ep, requested)
		if err != nil {
			return nil, err
		}
		return knowledgeBaseIDs(kbs), nil
	}
	if ep.RestrictsKnowledgeBases() {
		kbs, err := s.allowedKnowledgeBases(ctx, ep)
		if err != nil {
			return nil, err
		}
		return knowledgeBaseIDs(kbs), nil
	}
	mode := strings.ToLower(strings.TrimSpace(agent.Config.KBSelectionMode))
	agentHasNoKBs := mode == "none" || ((mode == "selected" || mode == "") && len(agent.Config.KnowledgeBases) == 0)
	if agentHasNoKBs {
		kbs, err := s.allowedKnowledgeBases(ctx, ep)
		if err != nil {
			return nil, err
		}
		return knowledgeBaseIDs(kbs), nil
	}
	// The agent has its own configured scope and nothing was selected. The
	// native QA path would read that configuration directly
	// (session_qa_helpers.go resolveKnowledgeBases -> agent); resolve it
	// here to the authorized canonical set so the binding compares the
	// effective scope instead of skipping the check.
	if agent.Config.RetrieveKBOnlyWhenMentioned {
		// With no mention on this surface native QA retrieves from nothing —
		// the suggestion anchor never reaches an MCP ask. The empty scope is
		// still a determined one: pin it, forward nothing.
		return []string{}, nil
	}
	allowed, err := s.allowedKnowledgeBases(ctx, ep)
	if err != nil {
		return nil, err
	}
	if mode == "all" {
		// Mirror the runtime's capability filter for "all" mode so the
		// pinned set is exactly what the retrieval would resolve itself.
		capFilter := tools.DeriveKBFilterForAgent(agent.Config.AgentMode, agent.Config.AllowedTools)
		kept := make([]*types.KnowledgeBase, 0, len(allowed))
		for _, kb := range allowed {
			if capFilter.IsEmpty() ||
				tools.KBSatisfiesAgentRequirements(
					kb.Capabilities(), agent.Config.AgentMode, agent.Config.AllowedTools) {
				kept = append(kept, kb)
			}
		}
		return knowledgeBaseIDs(kept), nil
	}
	// "selected", unset and unknown modes take the configured libraries, but
	// re-authorized: an entry this caller cannot read here is dropped rather
	// than trusted (it would also fail inside native retrieval).
	ids := make([]string, 0, len(agent.Config.KnowledgeBases))
	for _, configured := range agent.Config.KnowledgeBases {
		kb := matchKnowledgeBase(allowed, strings.TrimSpace(configured))
		if kb == nil {
			logger.Warnf(ctx, "[mcpserver] ask agent %s references unavailable knowledge base %q: dropped from the pinned scope",
				agent.ID, configured)
			continue
		}
		ids = append(ids, kb.ID)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf(
			"the endpoint's default agent has no readable knowledge base on this endpoint; select knowledge_base_ids explicitly")
	}
	return ids, nil
}

func (s *Server) resolveAskSession(
	ctx context.Context, ep *types.MCPEndpoint, sessionID, question string,
) (*types.Session, error) {
	// The owner must be the CALLER's session-scoped identity, not
	// unconditionally the endpoint's machine principal. runQA's
	// CreateMessage resolves the session through
	// types.SessionOwnerIDFromContext(ctx): in machine mode that is the
	// endpoint principal (matching the fallback below), but in user mode
	// it is the real user's id — binding the endpoint principal there made
	// every user-mode ask fail with "failed to create user message: session
	// not found", the same mismatch types.SessionOwnerIDFromContext
	// documents for the machine-mode half of this bug.
	ownerID := types.SessionOwnerIDFromContext(ctx)
	if ownerID == "" {
		ownerID = types.MCPEndpointPrincipal(ep.TenantID, ep.ID).StorageID()
	}
	if sessionID = strings.TrimSpace(sessionID); sessionID != "" {
		session, err := s.sessionService.GetSessionByID(ctx, ep.TenantID, sessionID)
		if err != nil || session == nil || session.UserID != ownerID {
			return nil, fmt.Errorf("session %q was not found on this endpoint", sessionID)
		}
		return session, nil
	}
	// Sharing the creator with the core surface keeps the title cap, the
	// description and the owner binding identical on both entry points.
	return s.createAskSession(ctx, ep, question)
}

// runQA executes one turn synchronously and returns the final answer and
// the references it cited. It mirrors the IM channel's non-streaming path.
func (s *Server) runQA(
	ctx context.Context, session *types.Session, agent *types.CustomAgent, question string, kbIDs []string,
) (string, []askReference, error) {
	ctx, cancel := context.WithTimeout(ctx, askTimeout)
	defer cancel()

	requestID := uuid.New().String()
	userMsg, err := s.messageService.CreateMessage(ctx, &types.Message{
		SessionID:   session.ID,
		Role:        "user",
		Content:     question,
		RequestID:   requestID,
		CreatedAt:   time.Now(),
		IsCompleted: true,
		Channel:     askChannel,
	})
	if err != nil {
		return "", nil, fmt.Errorf("failed to create user message: %w", err)
	}
	assistantMsg, err := s.messageService.CreateMessage(ctx, &types.Message{
		SessionID:   session.ID,
		Role:        "assistant",
		RequestID:   requestID,
		CreatedAt:   time.Now(),
		IsCompleted: false,
		Channel:     askChannel,
	})
	if err != nil {
		return "", nil, fmt.Errorf("failed to create assistant message: %w", err)
	}

	var (
		mu       sync.Mutex
		answer   strings.Builder
		final    string
		refs     []*types.SearchResult
		qaErr    error
		done     = make(chan struct{})
		complete = make(chan struct{})
		doneOnce sync.Once
		compOnce sync.Once
	)
	closeDone := func() { doneOnce.Do(func() { close(done) }) }
	closeComplete := func() { compOnce.Do(func() { close(complete) }) }

	bus := event.NewEventBus()
	bus.On(event.EventAgentFinalAnswer, func(_ context.Context, evt event.Event) error {
		data, ok := evt.Data.(event.AgentFinalAnswerData)
		if !ok {
			return nil
		}
		mu.Lock()
		answer.WriteString(data.Content)
		mu.Unlock()
		if data.Done {
			closeDone()
		}
		return nil
	})
	bus.On(event.EventAgentReferences, func(_ context.Context, evt event.Event) error {
		data, ok := evt.Data.(event.AgentReferencesData)
		if !ok {
			return nil
		}
		mu.Lock()
		collectReferences(&refs, data.References)
		mu.Unlock()
		return nil
	})
	bus.On(event.EventAgentComplete, func(_ context.Context, evt event.Event) error {
		data, ok := evt.Data.(event.AgentCompleteData)
		if !ok {
			return nil
		}
		mu.Lock()
		if data.FinalAnswer != "" {
			final = data.FinalAnswer
		}
		if len(data.KnowledgeRefs) > 0 {
			var completeRefs []*types.SearchResult
			collectReferences(&completeRefs, data.KnowledgeRefs)
			if len(completeRefs) > 0 {
				refs = completeRefs
			}
		}
		mu.Unlock()
		closeComplete()
		return nil
	})
	bus.On(event.EventError, func(_ context.Context, evt event.Event) error {
		data, ok := evt.Data.(event.ErrorData)
		if !ok {
			return nil
		}
		mu.Lock()
		qaErr = fmt.Errorf("%s", data.Error)
		mu.Unlock()
		closeDone()
		closeComplete()
		return nil
	})

	useAgent := agent.IsAgentMode()
	go func() {
		if useAgent {
			defer closeDone()
			defer closeComplete()
		}
		req := &types.QARequest{
			Session:            session,
			Query:              question,
			AssistantMessageID: assistantMsg.ID,
			UserMessageID:      userMsg.ID,
			CustomAgent:        agent,
			KnowledgeBaseIDs:   kbIDs,
			// Public web search is retired entirely; the field stays set to
			// false so the request shape is explicit about it.
			WebSearchEnabled: false,
		}
		var runErr error
		if useAgent {
			runErr = s.sessionService.AgentQA(ctx, req, bus)
		} else {
			runErr = s.sessionService.KnowledgeQA(ctx, req, bus)
		}
		if runErr != nil {
			mu.Lock()
			if qaErr == nil {
				qaErr = runErr
			}
			mu.Unlock()
			closeDone()
			closeComplete()
		}
	}()

	select {
	case <-done:
		if useAgent {
			timer := time.NewTimer(askCompleteWaitTimeout)
			select {
			case <-complete:
			case <-ctx.Done():
			case <-timer.C:
				logger.Warnf(ctx, "[mcpserver] ask timed out waiting for agent completion: session=%s", session.ID)
			}
			timer.Stop()
		}
	case <-ctx.Done():
		assistantMsg.Content = "The answer was interrupted before completion."
		assistantMsg.IsCompleted = true
		if updateErr := s.messageService.UpdateMessage(context.WithoutCancel(ctx), assistantMsg); updateErr != nil {
			logger.Warnf(ctx, "[mcpserver] failed to finalize interrupted message: %v", updateErr)
		}
		return "", nil, fmt.Errorf("answer timed out: %w", ctx.Err())
	}

	mu.Lock()
	text := answer.String()
	if strings.TrimSpace(text) == "" && final != "" {
		text = final
	}
	collected := append([]*types.SearchResult(nil), refs...)
	runErr := qaErr
	mu.Unlock()

	if strings.TrimSpace(text) == "" && runErr != nil {
		assistantMsg.Content = "The answer failed: " + runErr.Error()
		assistantMsg.IsCompleted = true
		if updateErr := s.messageService.UpdateMessage(context.WithoutCancel(ctx), assistantMsg); updateErr != nil {
			logger.Warnf(ctx, "[mcpserver] failed to finalize failed message: %v", updateErr)
		}
		return "", nil, runErr
	}
	if strings.TrimSpace(text) == "" {
		text = "No answer could be produced for this question."
	}
	assistantMsg.Content = text
	assistantMsg.IsCompleted = true
	if len(collected) > 0 {
		assistantMsg.KnowledgeReferences = types.References(collected)
	}
	if err := s.messageService.UpdateMessage(context.WithoutCancel(ctx), assistantMsg); err != nil {
		logger.Warnf(ctx, "[mcpserver] failed to persist assistant message: %v", err)
	}
	return text, summarizeReferences(collected), nil
}

func collectReferences(dst *[]*types.SearchResult, raw interface{}) {
	switch v := raw.(type) {
	case []*types.SearchResult:
		*dst = append(*dst, v...)
	case []interface{}:
		for _, item := range v {
			if sr, ok := item.(*types.SearchResult); ok {
				*dst = append(*dst, sr)
			}
		}
	}
}

const askExcerptMaxRunes = 300

func summarizeReferences(refs []*types.SearchResult) []askReference {
	out := make([]askReference, 0, len(refs))
	seen := map[string]int{}
	for _, r := range refs {
		if r == nil {
			continue
		}
		key := r.ID
		if key == "" {
			key = r.KnowledgeID + ":" + fmt.Sprint(r.ChunkIndex)
		}
		if index, dup := seen[key]; dup {
			out[index].Images = summarizeReferenceImages(r.ImageInfo, out[index].Images)
			continue
		}
		seen[key] = len(out)
		out = append(out, askReference{
			KnowledgeID:    r.KnowledgeID,
			KnowledgeTitle: r.KnowledgeTitle,
			ChunkID:        r.ID,
			Score:          r.Score,
			Excerpt:        truncateExcerpt(strings.TrimSpace(r.Content)),
			Images:         summarizeReferenceImages(r.ImageInfo, nil),
		})
	}
	return out
}

// truncateExcerpt cuts at askExcerptMaxRunes but never inside a storage
// reference: half a handle is unusable and would only cost a failed lookup.
// Whole image references are kept in askReference.Images instead.
func truncateExcerpt(excerpt string) string {
	runes := []rune(excerpt)
	if len(runes) <= askExcerptMaxRunes {
		return excerpt
	}
	cut := len(string(runes[:askExcerptMaxRunes]))
	for _, loc := range types.StorageReferencePattern.FindAllStringIndex(excerpt, -1) {
		if loc[0] >= cut {
			break
		}
		if cut < loc[1] {
			cut = loc[0]
			break
		}
	}
	return excerpt[:cut] + "…"
}

func summarizeReferenceImages(raw string, out []askImage) []askImage {
	var infos []types.ImageInfo
	if err := json.Unmarshal([]byte(raw), &infos); err != nil {
		return out
	}
	seen := make(map[string]int, len(out)+len(infos))
	for i, img := range out {
		seen[img.URL] = i
	}
	for _, info := range infos {
		url := strings.TrimSpace(info.URL)
		if url == "" {
			continue
		}
		if index, dup := seen[url]; dup {
			if out[index].Caption == "" {
				out[index].Caption = info.Caption
			}
			if out[index].OCRText == "" {
				out[index].OCRText = info.OCRText
			}
			continue
		}
		seen[url] = len(out)
		out = append(out, askImage{URL: url, Caption: info.Caption, OCRText: info.OCRText})
	}
	return out
}
