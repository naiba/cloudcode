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

	// 使用 context.Background 而非请求 ctx，因为 SSE 长连接不应随请求结束
	streamCtx, cancel := context.WithCancel(context.Background())
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
// 所有 SSE 事件都包裹在 {type, properties} 结构中

type sseEvent struct {
	Event string
	Data  string
}

// sseEnvelope 是 opencode SSE 事件的通用外层结构
// 所有事件数据都在 properties 字段中，不在顶层
type sseEnvelope struct {
	Type       string          `json:"type"`
	Properties json.RawMessage `json:"properties"`
}

// --- message.part.updated 事件 payload（流式文本来源）---

// partUpdatedProperties 对应 EventMessagePartUpdated.properties
type partUpdatedProperties struct {
	Part  partInfo `json:"part"`
	Delta string   `json:"delta,omitempty"` // 增量文本
}

type partInfo struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionID"`
	MessageID string `json:"messageID"`
	Type      string `json:"type"` // "text", "tool", etc.
	Text      string `json:"text"` // 完整文本（TextPart 才有）
}

// --- message.updated 事件 payload（消息元信息，非流式内容）---

// messageInfo 对应 EventMessageUpdated.properties.info
type messageInfo struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionID"`
	Role      string `json:"role"` // "assistant", "user"
}

type messageUpdatedProperties struct {
	Info messageInfo `json:"info"`
}

// --- session 事件 payload ---

type sessionIdleProperties struct {
	SessionID string `json:"sessionID"`
}

// session.error 的 error 字段可能是多种类型的结构体，这里简化处理
type sessionErrorProperties struct {
	SessionID string      `json:"sessionID,omitempty"`
	Error     interface{} `json:"error,omitempty"` // 可能是 object 或 string
}

// --- permission.updated 事件 payload ---
// 对应 EventPermissionUpdated.properties = Permission

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
	// Increase buffer for potentially large SSE events
	scanner.Buffer(make([]byte, 0, 256*1024), 256*1024)

	var currentEvent sseEvent
	// Track streaming state for sendMessageDraft
	var (
		lastDraftText string
		lastDraftTime time.Time
		draftID       string // unique per assistant message part
		lastMessageID string // 当前 assistant 消息 ID
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
				sm.handleEvent(ctx, &currentEvent, sessionID, topicID, inst,
					&lastDraftText, &lastDraftTime, &draftID, &lastMessageID)
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

	log.Printf("[telegram/stream] SSE disconnected for session %s", sessionID[:8])
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
	lastMessageID *string,
) {
	// opencode SSE 事件名直接在 event: 行中，data 是 JSON
	// 但有些版本 event 名在 data JSON 的 type 字段中
	// 优先使用 event: 行的事件名

	switch event.Event {
	case "message.part.updated":
		// 流式文本的核心事件：包含 part（完整文本）和 delta（增量文本）
		sm.handlePartUpdated(ctx, event.Data, sessionID, topicID,
			lastDraftText, lastDraftTime, draftID, lastMessageID)

	case "message.updated":
		// 消息元信息更新（role, tokens, cost 等），不包含文本内容
		// 仅用于检测新的 assistant 消息开始
		sm.handleMessageUpdated(ctx, event.Data, sessionID, topicID,
			lastDraftText, lastDraftTime, draftID, lastMessageID)

	case "session.idle":
		var env sseEnvelope
		json.Unmarshal([]byte(event.Data), &env)
		var props sessionIdleProperties
		json.Unmarshal(env.Properties, &props)

		// Only handle events for our session
		if props.SessionID != "" && props.SessionID != sessionID {
			return
		}

		log.Printf("[telegram/stream] session.idle for %s", sessionID[:8])

		// Send final message if we were streaming a draft
		if *lastDraftText != "" {
			sm.sendFinalMessage(ctx, topicID, *lastDraftText)
			*lastDraftText = ""
			*draftID = ""
			*lastMessageID = ""
		}

		// Close the SSE connection — session is done (on-demand, not persistent)
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

	case "permission.updated":
		// 权限请求事件：properties 直接是 Permission 对象
		var env sseEnvelope
		json.Unmarshal([]byte(event.Data), &env)
		var props permissionProperties
		json.Unmarshal(env.Properties, &props)

		if props.SessionID != "" && props.SessionID != sessionID {
			return
		}

		log.Printf("[telegram/stream] permission.updated for %s: %s", sessionID[:8], props.Title)

		text := fmt.Sprintf("🔐 *Permission Required*\n\n`%s`", props.Title)

		// 回调数据格式: perm:{action}:{instanceID}:{sessionID}:{permissionID}
		// 必须包含 permissionID 才能正确响应权限请求
		sm.bot.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:          sm.chatID,
			MessageThreadID: topicID,
			Text:            text,
			ParseMode:       models.ParseModeMarkdown,
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
// 每个 part 更新包含完整的 part 文本和可选的 delta 增量。
func (sm *StreamManager) handlePartUpdated(
	ctx context.Context,
	data string,
	sessionID string,
	topicID int,
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

	// 只处理我们 session 的 text 类型 part
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
		// 如果有旧 draft，先发送最终消息
		if *lastDraftText != "" {
			sm.sendFinalMessage(ctx, topicID, *lastDraftText)
		}
		*lastMessageID = props.Part.MessageID
		*draftID = fmt.Sprintf("draft-%s-%s", props.Part.MessageID, props.Part.ID)
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

// handleMessageUpdated processes message.updated — metadata about messages.
// 这个事件不包含消息文本，只有元信息（role, cost, tokens 等）。
// 我们用它来检测 session 过滤和日志。
func (sm *StreamManager) handleMessageUpdated(
	ctx context.Context,
	data string,
	sessionID string,
	topicID int,
	lastDraftText *string,
	lastDraftTime *time.Time,
	draftID *string,
	lastMessageID *string,
) {
	var env sseEnvelope
	if err := json.Unmarshal([]byte(data), &env); err != nil {
		return
	}

	var props messageUpdatedProperties
	if err := json.Unmarshal(env.Properties, &props); err != nil {
		return
	}

	// 只关注 assistant 消息
	if props.Info.Role != "assistant" {
		return
	}
	if props.Info.SessionID != "" && props.Info.SessionID != sessionID {
		return
	}

	// message.updated 不包含文本内容，文本在 message.part.updated 中
	// 这里仅作日志记录
	log.Printf("[telegram/stream] message.updated: assistant msg %s for session %s", props.Info.ID, sessionID[:8])
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
