package telegram

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/naiba/cloudcode/internal/store"
)

// Handler processes Telegram commands and forwards messages to opencode.
type Handler struct {
	parent *Bot
}

// NewHandler creates a Handler tied to the parent Bot.
func NewHandler(parent *Bot) *Handler {
	return &Handler{parent: parent}
}

// opencodeURL builds the internal Docker network URL for an opencode instance.
func opencodeURL(instanceID string, port int, path string) string {
	return fmt.Sprintf("http://cloudcode-%s:%d%s", instanceID, port, path)
}

// --- opencode API response types ---

type opencodeSession struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type opencodeMessagePart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type opencodeMessageBody struct {
	Parts []opencodeMessagePart `json:"parts"`
}

// handleNew processes /new [instance-name] command.
// Creates an opencode session and a forum topic mapped to it.
func (h *Handler) handleNew(ctx context.Context, msg *models.Message) {
	b := h.parent

	// Parse optional instance name from command args
	args := ""
	if parts := strings.SplitN(msg.Text, " ", 2); len(parts) > 1 {
		args = strings.TrimSpace(parts[1])
	}

	instances := b.getInstances()
	var running []*store.Instance
	for _, inst := range instances {
		if inst.Status == "running" {
			running = append(running, inst)
		}
	}

	if len(running) == 0 {
		h.reply(ctx, msg, "❌ No running instances")
		return
	}

	var target *store.Instance
	if args != "" {
		// Find by name
		for _, inst := range running {
			if inst.Name == args {
				target = inst
				break
			}
		}
		if target == nil {
			h.reply(ctx, msg, fmt.Sprintf("❌ Instance %q not found or not running", args))
			return
		}
	} else if len(running) == 1 {
		target = running[0]
	} else {
		// Multiple running instances — show inline keyboard
		var rows [][]models.InlineKeyboardButton
		for _, inst := range running {
			rows = append(rows, []models.InlineKeyboardButton{
				{Text: inst.Name, CallbackData: "new:" + inst.ID},
			})
		}
		b.bot.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:          b.chatID,
			MessageThreadID: msg.MessageThreadID,
			Text:            "Choose an instance:",
			ReplyMarkup: &models.InlineKeyboardMarkup{
				InlineKeyboard: rows,
			},
		})
		return
	}

	h.createSessionAndTopic(ctx, msg.MessageThreadID, target)
}

// handleNewCallback processes the inline keyboard callback for /new.
func (h *Handler) handleNewCallback(ctx context.Context, _ *bot.Bot, update *models.Update) {
	b := h.parent
	cb := update.CallbackQuery

	// Auth check
	if cb.Message.Message != nil && cb.Message.Message.Chat.ID != b.chatID {
		return
	}

	b.bot.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
		CallbackQueryID: cb.ID,
	})

	instanceID := strings.TrimPrefix(cb.Data, "new:")
	inst, err := b.store.Get(instanceID)
	if err != nil {
		log.Printf("[telegram] callback: instance %s not found: %v", instanceID, err)
		return
	}

	threadID := 0
	if cb.Message.Message != nil {
		threadID = cb.Message.Message.MessageThreadID
	}

	h.createSessionAndTopic(ctx, threadID, inst)
}

// createSessionAndTopic creates an opencode session, a forum topic, and stores the mapping.
func (h *Handler) createSessionAndTopic(ctx context.Context, replyThreadID int, inst *store.Instance) {
	b := h.parent

	// Create opencode session
	sess, err := h.createOpencodeSession(ctx, inst, "")
	if err != nil {
		h.send(ctx, replyThreadID, fmt.Sprintf("❌ Failed to create session: %v", err))
		return
	}

	topicTitle := sess.Title
	if topicTitle == "" {
		topicTitle = "New Session"
	}
	// Prefix with instance name for clarity
	topicTitle = fmt.Sprintf("[%s] %s", inst.Name, topicTitle)

	// Create forum topic
	topic, err := b.bot.CreateForumTopic(ctx, &bot.CreateForumTopicParams{
		ChatID: b.chatID,
		Name:   truncateTopicName(topicTitle),
	})
	if err != nil {
		h.send(ctx, replyThreadID, fmt.Sprintf("❌ Failed to create topic: %v", err))
		return
	}

	// Store mapping
	ts := &store.TopicSession{
		TopicID:    topic.MessageThreadID,
		InstanceID: inst.ID,
		SessionID:  sess.ID,
	}
	if err := b.store.CreateTopicSession(ts); err != nil {
		log.Printf("[telegram] failed to store topic-session mapping: %v", err)
	}

	h.send(ctx, topic.MessageThreadID,
		fmt.Sprintf("✅ Session created\n\n**Instance:** %s\n**Session:** `%s`", inst.Name, sess.ID))
}

// handleAbort processes /abort in a session topic.
func (h *Handler) handleAbort(ctx context.Context, msg *models.Message) {
	b := h.parent

	if msg.MessageThreadID == 0 {
		h.reply(ctx, msg, "❌ Use /abort inside a session topic")
		return
	}

	ts, err := b.store.GetTopicSession(msg.MessageThreadID)
	if err != nil {
		h.reply(ctx, msg, "❌ No session linked to this topic")
		return
	}

	inst, err := b.store.Get(ts.InstanceID)
	if err != nil {
		h.reply(ctx, msg, "❌ Instance not found")
		return
	}

	url := opencodeURL(inst.ID, inst.Port, "/session/"+ts.SessionID+"/abort")
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.reply(ctx, msg, fmt.Sprintf("❌ Abort failed: %v", err))
		return
	}
	resp.Body.Close()

	h.reply(ctx, msg, "✅ Session aborted")
}

// handleList processes /list — shows all active topic-session mappings.
func (h *Handler) handleList(ctx context.Context, msg *models.Message) {
	b := h.parent

	instances := b.getInstances()
	if len(instances) == 0 {
		h.reply(ctx, msg, "No instances found")
		return
	}

	var sb strings.Builder
	sb.WriteString("**Active Sessions**\n\n")
	totalSessions := 0

	for _, inst := range instances {
		sessions, err := b.store.ListTopicSessionsByInstance(inst.ID)
		if err != nil || len(sessions) == 0 {
			continue
		}
		sb.WriteString(fmt.Sprintf("📦 **%s** (%s)\n", inst.Name, inst.Status))
		for _, ts := range sessions {
			totalSessions++
			sessID := ts.SessionID
			if len(sessID) > 8 {
				sessID = sessID[:8] + "…"
			}
			sb.WriteString(fmt.Sprintf("  └ topic:%d → `%s`\n", ts.TopicID, sessID))
		}
		sb.WriteString("\n")
	}

	if totalSessions == 0 {
		h.reply(ctx, msg, "No active sessions")
		return
	}

	h.reply(ctx, msg, sb.String())
}

// handleSessionMessage forwards a regular message in a session topic to opencode.
func (h *Handler) handleSessionMessage(ctx context.Context, msg *models.Message) {
	b := h.parent

	if msg.Text == "" {
		return
	}

	ts, err := b.store.GetTopicSession(msg.MessageThreadID)
	if err == sql.ErrNoRows {
		// Not a session topic — ignore
		return
	}
	if err != nil {
		log.Printf("[telegram] get topic session: %v", err)
		return
	}

	inst, err := b.store.Get(ts.InstanceID)
	if err != nil {
		h.reply(ctx, msg, "❌ Instance not found")
		return
	}

	// If no session ID yet, create one first
	if ts.SessionID == "" {
		sess, err := h.createOpencodeSession(ctx, inst, "")
		if err != nil {
			h.reply(ctx, msg, fmt.Sprintf("❌ Failed to create session: %v", err))
			return
		}
		ts.SessionID = sess.ID
		if err := b.store.UpdateTopicSessionID(ts.TopicID, sess.ID); err != nil {
			log.Printf("[telegram] update session ID: %v", err)
		}
	}

	// Start SSE stream for this session (idempotent — won't duplicate)
	b.streams.EnsureStream(ctx, ts, inst)

	// Send message to opencode
	body := opencodeMessageBody{
		Parts: []opencodeMessagePart{
			{Type: "text", Text: msg.Text},
		},
	}
	bodyJSON, _ := json.Marshal(body)

	url := opencodeURL(inst.ID, inst.Port, "/session/"+ts.SessionID+"/message")
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyJSON))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.reply(ctx, msg, fmt.Sprintf("❌ Failed to send message: %v", err))
		return
	}
	resp.Body.Close()
}

// handlePermissionCallback processes allow/deny permission responses.
func (h *Handler) handlePermissionCallback(ctx context.Context, _ *bot.Bot, update *models.Update) {
	b := h.parent
	cb := update.CallbackQuery

	b.bot.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
		CallbackQueryID: cb.ID,
	})

	// Format: perm:allow:<instanceID>:<sessionID> or perm:deny:<instanceID>:<sessionID>
	parts := strings.SplitN(cb.Data, ":", 4)
	if len(parts) < 4 {
		return
	}
	action := parts[1]
	instanceID := parts[2]
	sessionID := parts[3]

	inst, err := b.store.Get(instanceID)
	if err != nil {
		return
	}

	// POST to opencode permission endpoint
	permBody := map[string]bool{"allow": action == "allow"}
	bodyJSON, _ := json.Marshal(permBody)

	url := opencodeURL(inst.ID, inst.Port, "/session/"+sessionID+"/permission")
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyJSON))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("[telegram] permission response failed: %v", err)
		return
	}
	resp.Body.Close()

	// Edit the original message to show the decision
	statusText := "✅ Allowed"
	if action != "allow" {
		statusText = "❌ Denied"
	}
	if cb.Message.Message != nil {
		b.bot.EditMessageText(ctx, &bot.EditMessageTextParams{
			ChatID:    b.chatID,
			MessageID: cb.Message.Message.ID,
			Text:      cb.Message.Message.Text + "\n\n" + statusText,
		})
	}
}


// handleSync synchronizes Telegram topics with opencode sessions:
// - Deletes topics for sessions that no longer exist in opencode
// - Creates topics for sessions that exist in opencode but have no topic
func (h *Handler) handleSync(ctx context.Context, msg *models.Message) {
	b := h.parent
	instances := b.getInstances()
	var running []*store.Instance
	for _, inst := range instances {
		if inst.Status == "running" {
			running = append(running, inst)
		}
	}

	if len(running) == 0 {
		h.reply(ctx, msg, "❌ No running instances")
		return
	}

	var sb strings.Builder
	var removedCount, createdCount int

	for _, inst := range running {
		// Get all opencode sessions for this instance
		activeSessions, err := h.listOpencodeSessions(ctx, inst)
		if err != nil {
			sb.WriteString(fmt.Sprintf("⚠️ %s: failed to list sessions: %v\n", inst.Name, err))
			continue
		}

		// Build set of active session IDs
		activeSet := make(map[string]*opencodeSession, len(activeSessions))
		for i := range activeSessions {
			activeSet[activeSessions[i].ID] = &activeSessions[i]
		}

		// 1. Remove topics for deleted sessions
		topicSessions, _ := b.store.ListTopicSessionsByInstance(inst.ID)
		for _, ts := range topicSessions {
			if ts.SessionID == "" {
				continue
			}
			if _, exists := activeSet[ts.SessionID]; !exists {
				// Session no longer exists — delete topic and mapping
				// DeleteForumTopic may fail if topic was already deleted; ignore errors
				b.bot.DeleteForumTopic(ctx, &bot.DeleteForumTopicParams{
					ChatID:          b.chatID,
					MessageThreadID: ts.TopicID,
				})
				_ = b.store.DeleteTopicSession(ts.TopicID)
				removedCount++
			}
		}

		// 2. Create topics for sessions without one
		// Build set of session IDs that already have topics
		mappedSessions := make(map[string]bool)
		// Re-query after deletions
		topicSessions, _ = b.store.ListTopicSessionsByInstance(inst.ID)
		for _, ts := range topicSessions {
			if ts.SessionID != "" {
				mappedSessions[ts.SessionID] = true
			}
		}

		for sessID, sess := range activeSet {
			if mappedSessions[sessID] {
				continue
			}
			// Create topic for this session
			topicTitle := sess.Title
			if topicTitle == "" {
				topicTitle = sessID[:8]
			}
			topicTitle = fmt.Sprintf("[%s] %s", inst.Name, topicTitle)

			topic, err := b.bot.CreateForumTopic(ctx, &bot.CreateForumTopicParams{
				ChatID: b.chatID,
				Name:   truncateTopicName(topicTitle),
			})
			if err != nil {
				sb.WriteString(fmt.Sprintf("⚠️ Failed to create topic for session %s: %v\n", sessID[:8], err))
				continue
			}

			ts := &store.TopicSession{
				TopicID:    topic.MessageThreadID,
				InstanceID: inst.ID,
				SessionID:  sessID,
			}
			_ = b.store.CreateTopicSession(ts)
			createdCount++
		}
	}

	result := fmt.Sprintf("✅ Sync complete: %d topics removed, %d topics created", removedCount, createdCount)
	if sb.Len() > 0 {
		result += "\n\n" + sb.String()
	}
	h.reply(ctx, msg, result)
}
// --- Helpers ---

// listOpencodeSessions calls GET /session on the opencode API.
func (h *Handler) listOpencodeSessions(ctx context.Context, inst *store.Instance) ([]opencodeSession, error) {
	url := opencodeURL(inst.ID, inst.Port, "/session")
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
	}

	var sessions []opencodeSession
	if err := json.NewDecoder(resp.Body).Decode(&sessions); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return sessions, nil
}

// createOpencodeSession calls POST /session on the opencode API.
func (h *Handler) createOpencodeSession(ctx context.Context, inst *store.Instance, title string) (*opencodeSession, error) {
	reqBody := map[string]string{}
	if title != "" {
		reqBody["title"] = title
	}
	bodyJSON, _ := json.Marshal(reqBody)

	url := opencodeURL(inst.ID, inst.Port, "/session")
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyJSON))
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
	}

	var sess opencodeSession
	if err := json.NewDecoder(resp.Body).Decode(&sess); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &sess, nil
}

// reply sends a message in the same thread as msg.
func (h *Handler) reply(ctx context.Context, msg *models.Message, text string) {
	h.send(ctx, msg.MessageThreadID, text)
}

// send sends a message to a specific thread.
func (h *Handler) send(ctx context.Context, threadID int, text string) {
	b := h.parent
	b.bot.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:          b.chatID,
		MessageThreadID: threadID,
		Text:            text,
		ParseMode:       models.ParseModeMarkdown,
	})
}


const helpText = `*CloudCode Bot*

/new [instance] — Create a new session + topic
/abort — Abort current session (in session topic)
/list — List active sessions
/sync — Sync topics with opencode sessions
/help — Show this help

Send any message in a session topic to chat with opencode.`

// handleHelp sends usage information.
func (h *Handler) handleHelp(ctx context.Context, msg *models.Message) {
	h.parent.bot.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:          h.parent.chatID,
		MessageThreadID: msg.MessageThreadID,
		Text:            helpText,
		ParseMode:       models.ParseModeMarkdown,
	})
}
// truncateTopicName limits topic name to Telegram's 128 char limit.
func truncateTopicName(name string) string {
	if len(name) > 128 {
		return name[:125] + "…"
	}
	return name
}
