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
func (sm *StreamManager) EnsureStream(inst *store.Instance, sessionID string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if _, ok := sm.streams[sessionID]; ok {
		return // already streaming
	}

	// 使用 context.Background 而非请求 ctx，因为 SSE 长连接不应随请求结束
	streamCtx, cancel := context.WithCancel(context.Background())
	sm.streams[sessionID] = cancel

	go sm.listenSSE(streamCtx, sessionID, inst)
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
// 所有 SSE 事件都包裹在 {type, properties} 结构中

type sseEvent struct {
	Event string
	Data  string
}

// sseEnvelope 是 opencode SSE 事件的通用外层结构
type sseEnvelope struct {
	Type       string          `json:"type"`
	Properties json.RawMessage `json:"properties"`
}

type partUpdatedProperties struct {
	Part  partInfo `json:"part"`
	Delta string   `json:"delta,omitempty"`
}

type partInfo struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionID"`
	MessageID string `json:"messageID"`
	Type      string `json:"type"`
	Text      string `json:"text"`
}

type messageInfo struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionID"`
	Role      string `json:"role"`
}

type messageUpdatedProperties struct {
	Info messageInfo `json:"info"`
}

type sessionIdleProperties struct {
	SessionID string `json:"sessionID"`
}

type sessionErrorProperties struct {
	SessionID string      `json:"sessionID,omitempty"`
	Error     interface{} `json:"error,omitempty"`
}

type permissionProperties struct {
	ID        string                 `json:"id"`
	Type      string                 `json:"type"`
	SessionID string                 `json:"sessionID"`
	MessageID string                 `json:"messageID"`
	CallID    string                 `json:"callID,omitempty"`
	Title     string                 `json:"title"`
	Metadata  map[string]interface{} `json:"metadata"`
}

// listenSSE connects to the opencode /event SSE endpoint and processes events.
func (sm *StreamManager) listenSSE(ctx context.Context, sessionID string, inst *store.Instance) {
	defer sm.Stop(sessionID)

	url := opencodeURL(inst.ID, inst.Port, "/event")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		log.Printf("[telegram/stream] create request: %v", err)
		return
	}
	req.Header.Set("Accept", "text/event-stream")

	client := &http.Client{Timeout: 0}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[telegram/stream] connect SSE for session %s: %v", sessionID[:8], err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("[telegram/stream] SSE returned %d for session %s", resp.StatusCode, sessionID[:8])
		return
	}

	log.Printf("[telegram/stream] SSE connected for session %s on instance %s", sessionID[:8], inst.ID)

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 256*1024), 256*1024)

	var currentEvent sseEvent
	var (
		lastDraftText string
		lastDraftTime time.Time
		draftID       string
		lastMessageID string
	)

	for scanner.Scan() {
		line := scanner.Text()

		if line == "" {
			// 如果没有 event: 行，从 data JSON 的 type 字段提取事件类型
			if currentEvent.Event == "" && currentEvent.Data != "" {
				var env sseEnvelope
				if json.Unmarshal([]byte(currentEvent.Data), &env) == nil && env.Type != "" {
					currentEvent.Event = env.Type
				}
			}
			if currentEvent.Event != "" && currentEvent.Data != "" {
				sm.handleEvent(ctx, &currentEvent, sessionID, inst,
					&lastDraftText, &lastDraftTime, &draftID, &lastMessageID)
			}
			currentEvent = sseEvent{}
			continue
		}

		if strings.HasPrefix(line, "event:") {
			currentEvent.Event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		} else if strings.HasPrefix(line, "data:") {
			data := strings.TrimPrefix(line, "data:")
			if currentEvent.Data != "" {
				currentEvent.Data += "\n"
			}
			currentEvent.Data += data
		}
	}

	log.Printf("[telegram/stream] SSE disconnected for session %s", sessionID[:8])
}

// handleEvent processes a single SSE event.
func (sm *StreamManager) handleEvent(
	ctx context.Context,
	event *sseEvent,
	sessionID string,
	inst *store.Instance,
	lastDraftText *string,
	lastDraftTime *time.Time,
	draftID *string,
	lastMessageID *string,
) {
	switch event.Event {
	case "message.part.updated":
		sm.handlePartUpdated(ctx, event.Data, sessionID,
			lastDraftText, lastDraftTime, draftID, lastMessageID)

	case "message.updated":
		sm.handleMessageUpdated(event.Data, sessionID)

	case "session.idle":
		var env sseEnvelope
		json.Unmarshal([]byte(event.Data), &env)
		var props sessionIdleProperties
		json.Unmarshal(env.Properties, &props)

		if props.SessionID != "" && props.SessionID != sessionID {
			return
		}

		log.Printf("[telegram/stream] session.idle for %s", sessionID[:8])

		if *lastDraftText != "" {
			sm.sendFinalMessage(ctx, *lastDraftText)
			*lastDraftText = ""
			*draftID = ""
			*lastMessageID = ""
		}

		sm.Stop(sessionID)

	case "session.error":
		var env sseEnvelope
		json.Unmarshal([]byte(event.Data), &env)
		var props sessionErrorProperties
		json.Unmarshal(env.Properties, &props)

		if props.SessionID != "" && props.SessionID != sessionID {
			return
		}

		log.Printf("[telegram/stream] session.error for %s: %v", sessionID[:8], props.Error)

		errMsg := "Unknown error"
		if props.Error != nil {
			errMsg = fmt.Sprintf("%v", props.Error)
		}

		if *lastDraftText != "" {
			sm.sendFinalMessage(ctx, *lastDraftText+"\n\n⚠️ "+errMsg)
			*lastDraftText = ""
		} else {
			sm.bot.SendMessage(ctx, &bot.SendMessageParams{
				ChatID: sm.chatID,
				Text:   "⚠️ " + errMsg,
			})
		}

		sm.Stop(sessionID)

	case "permission.updated":
		var env sseEnvelope
		json.Unmarshal([]byte(event.Data), &env)
		var props permissionProperties
		json.Unmarshal(env.Properties, &props)

		if props.SessionID != "" && props.SessionID != sessionID {
			return
		}

		log.Printf("[telegram/stream] permission.updated for %s: %s", sessionID[:8], props.Title)

		text := fmt.Sprintf("🔐 *Permission Required*\n\n`%s`", props.Title)

		sm.bot.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:    sm.chatID,
			Text:      text,
			ParseMode: models.ParseModeMarkdown,
			ReplyMarkup: &models.InlineKeyboardMarkup{
				InlineKeyboard: [][]models.InlineKeyboardButton{
					{
						{Text: "✅ Allow", CallbackData: fmt.Sprintf("perm:allow:%s:%s:%s", inst.ID, sessionID, props.ID)},
						{Text: "❌ Deny", CallbackData: fmt.Sprintf("perm:deny:%s:%s:%s", inst.ID, sessionID, props.ID)},
					},
				},
			},
		})
	}
}

// handlePartUpdated processes message.part.updated — the primary streaming text event.
func (sm *StreamManager) handlePartUpdated(
	ctx context.Context,
	data string,
	sessionID string,
	lastDraftText *string,
	lastDraftTime *time.Time,
	draftID *string,
	lastMessageID *string,
) {
	var env sseEnvelope
	if err := json.Unmarshal([]byte(data), &env); err != nil {
		return
	}

	var props partUpdatedProperties
	if err := json.Unmarshal(env.Properties, &props); err != nil {
		return
	}

	if props.Part.Type != "text" {
		return
	}
	if props.Part.SessionID != "" && props.Part.SessionID != sessionID {
		return
	}

	text := props.Part.Text
	if text == "" {
		return
	}

	// 新消息开始 → 新的 draft
	if props.Part.MessageID != *lastMessageID {
		if *lastDraftText != "" {
			sm.sendFinalMessage(ctx, *lastDraftText)
		}
		*lastMessageID = props.Part.MessageID
		*draftID = fmt.Sprintf("draft-%s-%s", props.Part.MessageID, props.Part.ID)
		*lastDraftText = ""
		*lastDraftTime = time.Time{}
	}

	*lastDraftText = text

	now := time.Now()
	if now.Sub(*lastDraftTime) < 500*time.Millisecond {
		return
	}
	*lastDraftTime = now

	displayText := text + " ▌"

	log.Printf("[telegram/stream] sending draft: %d chars", len(text))
	sm.bot.SendMessageDraft(ctx, &bot.SendMessageDraftParams{
		ChatID:  sm.chatID,
		DraftID: *draftID,
		Text:    displayText,
	})
}

// handleMessageUpdated processes message.updated — metadata only, for logging.
func (sm *StreamManager) handleMessageUpdated(data string, sessionID string) {
	var env sseEnvelope
	if err := json.Unmarshal([]byte(data), &env); err != nil {
		return
	}

	var props messageUpdatedProperties
	if err := json.Unmarshal(env.Properties, &props); err != nil {
		return
	}

	if props.Info.Role != "assistant" {
		return
	}
	if props.Info.SessionID != "" && props.Info.SessionID != sessionID {
		return
	}

	log.Printf("[telegram/stream] message.updated: assistant msg %s for session %s", props.Info.ID, sessionID[:8])
}

// sendFinalMessage sends the completed response.
func (sm *StreamManager) sendFinalMessage(ctx context.Context, text string) {
	if len(text) > 4000 {
		text = text[:4000] + "\n\n_(truncated)_"
	}

	log.Printf("[telegram/stream] sending final message: %d chars", len(text))
	_, err := sm.bot.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:    sm.chatID,
		Text:      text,
		ParseMode: models.ParseModeMarkdown,
	})
	if err != nil {
		log.Printf("[telegram/stream] sendFinalMessage failed: %v", err)
	}
}
