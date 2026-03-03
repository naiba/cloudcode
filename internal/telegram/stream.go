package telegram

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/naiba/cloudcode/internal/store"
)

// StreamManager tracks active SSE connections to opencode instances per session.
type StreamManager struct {
	mu      sync.Mutex
	streams map[string]context.CancelFunc // sessionID → cancel function
	bot     *bot.Bot
	chatID  int64
	store   *store.Store
}

// NewStreamManager creates a new StreamManager.
func NewStreamManager(s *store.Store) *StreamManager {
	return &StreamManager{
		streams: make(map[string]context.CancelFunc),
		store:   s,
	}
}

// EnsureStream starts an SSE listener for the session if one isn't already running.
func (sm *StreamManager) EnsureStream(ctx context.Context, ts *store.TopicSession, inst *store.Instance) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if _, ok := sm.streams[ts.SessionID]; ok {
		return // already streaming
	}

	streamCtx, cancel := context.WithCancel(ctx)
	sm.streams[ts.SessionID] = cancel

	go sm.listenSSE(streamCtx, ts.SessionID, ts.TopicID, inst)
}

// Stop cancels and removes the stream for a session.
func (sm *StreamManager) Stop(sessionID string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if cancel, ok := sm.streams[sessionID]; ok {
		cancel()
		delete(sm.streams, sessionID)
	}
}

// --- SSE event types from opencode ---

type sseEvent struct {
	Event string
	Data  string
}

// messageUpdatedPayload is the relevant part of a message.updated SSE event.
type messageUpdatedPayload struct {
	Type      string               `json:"type"` // "assistant", "user", etc.
	SessionID string               `json:"sessionID"`
	ID        string               `json:"id"`
	Parts     []messageUpdatedPart `json:"parts"`
}

type messageUpdatedPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// sessionEventPayload covers session.idle, session.error events.
type sessionEventPayload struct {
	SessionID string `json:"sessionID"`
	Error     string `json:"error,omitempty"`
}

// permissionAskedPayload covers permission.asked events.
type permissionAskedPayload struct {
	SessionID string `json:"sessionID"`
	ID        string `json:"id"`
	Title     string `json:"title"`
	Message   string `json:"message"`
}

// listenSSE connects to the opencode /event SSE endpoint and processes events.
func (sm *StreamManager) listenSSE(ctx context.Context, sessionID string, topicID int, inst *store.Instance) {
	defer sm.Stop(sessionID)

	url := opencodeURL(inst.ID, inst.Port, "/event")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		log.Printf("[telegram/stream] create request: %v", err)
		return
	}
	req.Header.Set("Accept", "text/event-stream")

	client := &http.Client{
		// No timeout — SSE is a long-lived connection
		Timeout: 0,
	}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[telegram/stream] connect SSE: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("[telegram/stream] SSE returned %d", resp.StatusCode)
		return
	}

	scanner := bufio.NewScanner(resp.Body)
	// Increase buffer for potentially large SSE events
	scanner.Buffer(make([]byte, 0, 256*1024), 256*1024)

	var currentEvent sseEvent
	// Track streaming state for sendMessageDraft
	var (
		lastDraftText   string
		lastDraftTime   time.Time
		draftID         string // unique per assistant message
		lastAssistantID string
	)

	for scanner.Scan() {
		line := scanner.Text()

		if line == "" {
			// Empty line = event boundary, dispatch
			if currentEvent.Event != "" && currentEvent.Data != "" {
				sm.handleEvent(ctx, &currentEvent, sessionID, topicID, inst,
					&lastDraftText, &lastDraftTime, &draftID, &lastAssistantID)
			}
			currentEvent = sseEvent{}
			continue
		}

		if strings.HasPrefix(line, "event:") {
			currentEvent.Event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		} else if strings.HasPrefix(line, "data:") {
			// Data can span multiple lines (though unusual for opencode)
			data := strings.TrimPrefix(line, "data:")
			if currentEvent.Data != "" {
				currentEvent.Data += "\n"
			}
			currentEvent.Data += data
		}
	}
}

// handleEvent processes a single SSE event.
func (sm *StreamManager) handleEvent(
	ctx context.Context,
	event *sseEvent,
	sessionID string,
	topicID int,
	inst *store.Instance,
	lastDraftText *string,
	lastDraftTime *time.Time,
	draftID *string,
	lastAssistantID *string,
) {
	switch event.Event {
	case "message.updated":
		sm.handleMessageUpdated(ctx, event.Data, sessionID, topicID,
			lastDraftText, lastDraftTime, draftID, lastAssistantID)

	case "session.idle":
		var payload sessionEventPayload
		json.Unmarshal([]byte(event.Data), &payload)
		// Only handle events for our session
		if payload.SessionID != "" && payload.SessionID != sessionID {
			return
		}

		// Send final message if we were streaming a draft
		if *lastDraftText != "" {
			sm.sendFinalMessage(ctx, topicID, *lastDraftText)
			*lastDraftText = ""
			*draftID = ""
			*lastAssistantID = ""
		}

		// Close the SSE connection — session is done (on-demand, not persistent)
		sm.Stop(sessionID)

	case "session.error":
		var payload sessionEventPayload
		json.Unmarshal([]byte(event.Data), &payload)
		if payload.SessionID != "" && payload.SessionID != sessionID {
			return
		}

		errMsg := payload.Error
		if errMsg == "" {
			errMsg = "Unknown error"
		}

		// Send final message with error
		if *lastDraftText != "" {
			sm.sendFinalMessage(ctx, topicID, *lastDraftText+"\n\n⚠️ "+errMsg)
			*lastDraftText = ""
		} else {
			sm.bot.SendMessage(ctx, &bot.SendMessageParams{
				ChatID:          sm.chatID,
				MessageThreadID: topicID,
				Text:            "⚠️ " + errMsg,
			})
		}

		sm.Stop(sessionID)

	case "permission.asked":
		var payload permissionAskedPayload
		json.Unmarshal([]byte(event.Data), &payload)
		if payload.SessionID != "" && payload.SessionID != sessionID {
			return
		}

		text := fmt.Sprintf("🔐 **Permission Required**\n\n%s", payload.Message)
		if payload.Title != "" {
			text = fmt.Sprintf("🔐 **%s**\n\n%s", payload.Title, payload.Message)
		}

		sm.bot.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:          sm.chatID,
			MessageThreadID: topicID,
			Text:            text,
			ParseMode:       models.ParseModeMarkdown,
			ReplyMarkup: &models.InlineKeyboardMarkup{
				InlineKeyboard: [][]models.InlineKeyboardButton{
					{
						{Text: "✅ Allow", CallbackData: fmt.Sprintf("perm:allow:%s:%s", inst.ID, sessionID)},
						{Text: "❌ Deny", CallbackData: fmt.Sprintf("perm:deny:%s:%s", inst.ID, sessionID)},
					},
				},
			},
		})
	}
}

// handleMessageUpdated processes assistant message streaming.
func (sm *StreamManager) handleMessageUpdated(
	ctx context.Context,
	data string,
	sessionID string,
	topicID int,
	lastDraftText *string,
	lastDraftTime *time.Time,
	draftID *string,
	lastAssistantID *string,
) {
	var payload messageUpdatedPayload
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		return
	}

	// Only stream assistant messages for our session
	if payload.Type != "assistant" {
		return
	}
	if payload.SessionID != "" && payload.SessionID != sessionID {
		return
	}

	// Extract text from parts
	var text string
	for _, part := range payload.Parts {
		if part.Type == "text" {
			text += part.Text
		}
	}

	if text == "" {
		return
	}

	// New assistant message → new draft ID
	if payload.ID != *lastAssistantID {
		// If we had a previous draft, finalize it
		if *lastDraftText != "" {
			sm.sendFinalMessage(ctx, topicID, *lastDraftText)
		}
		*lastAssistantID = payload.ID
		*draftID = fmt.Sprintf("draft-%s", payload.ID)
		*lastDraftText = ""
		*lastDraftTime = time.Time{}
	}

	*lastDraftText = text

	// Throttle: send draft every 500ms to avoid rate limits
	now := time.Now()
	if now.Sub(*lastDraftTime) < 500*time.Millisecond {
		return
	}
	*lastDraftTime = now

	// Append cursor indicator while streaming
	displayText := text + " ▌"

	sm.bot.SendMessageDraft(ctx, &bot.SendMessageDraftParams{
		ChatID:          sm.chatID,
		MessageThreadID: topicID,
		DraftID:         *draftID,
		Text:            displayText,
	})
}

// sendFinalMessage sends the completed response via sendMessage.
// The draft disappears automatically when we send the final message.
func (sm *StreamManager) sendFinalMessage(ctx context.Context, topicID int, text string) {
	// Telegram has a 4096 char message limit; truncate if needed
	if len(text) > 4000 {
		text = text[:4000] + "\n\n_(truncated)_"
	}

	sm.bot.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:          sm.chatID,
		MessageThreadID: topicID,
		Text:            text,
		ParseMode:       models.ParseModeMarkdown,
	})
}
