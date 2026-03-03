package telegram

import (
	"bytes"
	"context"
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

// opencodeURL builds the internal Docker network URL for an opencode instance.
func opencodeURL(instanceID string, port int, path string) string {
	return fmt.Sprintf("http://cloudcode-%s:%d%s", instanceID, port, path)
}

// --- Command Handlers ---

const helpText = `*CloudCode Bot*

/cc\_new \[instance\] — Create a new session
/cc\_select — Select an existing session
/cc\_list — List all sessions
/cc\_abort — Abort current session
/cc\_exit — Disconnect from session

Send any message to chat with the active session.`

func (b *Bot) handleHelp(ctx context.Context, msg *models.Message) {
	b.send(ctx, helpText)
}

// handleNew 创建新 session 并自动对接
func (b *Bot) handleNew(ctx context.Context, msg *models.Message) {
	args := ""
	if parts := strings.SplitN(msg.Text, " ", 2); len(parts) > 1 {
		args = strings.TrimSpace(parts[1])
	}

	running := b.runningInstances()
	if len(running) == 0 {
		b.send(ctx, "❌ No running instances")
		return
	}

	var target *store.Instance
	if args != "" {
		for _, inst := range running {
			if inst.Name == args {
				target = inst
				break
			}
		}
		if target == nil {
			b.send(ctx, fmt.Sprintf("❌ Instance %q not found or not running", args))
			return
		}
	} else if len(running) == 1 {
		target = running[0]
	} else {
		// 多实例 → inline keyboard 选择
		var rows [][]models.InlineKeyboardButton
		for _, inst := range running {
			rows = append(rows, []models.InlineKeyboardButton{
				{Text: inst.Name, CallbackData: "new:" + inst.ID},
			})
		}
		b.bot.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: b.chatID,
			Text:   "Choose an instance:",
			ReplyMarkup: &models.InlineKeyboardMarkup{
				InlineKeyboard: rows,
			},
		})
		return
	}

	b.createSessionAndActivate(ctx, target)
}

// handleNewCallback 处理 /cc-new 的 inline keyboard 回调
func (b *Bot) handleNewCallback(ctx context.Context, _ *bot.Bot, update *models.Update) {
	cb := update.CallbackQuery
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

	b.createSessionAndActivate(ctx, inst)
}

// createSessionAndActivate 创建 session 并设为活跃
func (b *Bot) createSessionAndActivate(ctx context.Context, inst *store.Instance) {
	sess, err := b.createOpencodeSession(ctx, inst)
	if err != nil {
		b.send(ctx, fmt.Sprintf("❌ Failed to create session: %v", err))
		return
	}

	// 停止旧 session 的 SSE
	b.ClearActive()
	b.SetActive(inst.ID, sess.ID)

	title := sess.Title
	if title == "" {
		title = sess.ID[:8]
	}

	b.send(ctx, fmt.Sprintf("✅ Session created and activated\n\n*Instance:* %s\n*Session:* `%s`\n*Title:* %s\n\nSend a message to start chatting.", inst.Name, sess.ID[:8], title))

	// 启动 SSE 流
	b.streams.EnsureStream(inst, sess.ID)
}

// handleSelect 列出所有 session 供用户选择
func (b *Bot) handleSelect(ctx context.Context, msg *models.Message) {
	running := b.runningInstances()
	if len(running) == 0 {
		b.send(ctx, "❌ No running instances")
		return
	}

	var rows [][]models.InlineKeyboardButton
	for _, inst := range running {
		sessions, err := b.listOpencodeSessions(ctx, inst)
		if err != nil {
			continue
		}
		for _, sess := range sessions {
			title := sess.Title
			if title == "" {
				title = sess.ID[:8]
			}
			label := fmt.Sprintf("[%s] %s", inst.Name, title)
			if len(label) > 64 {
				label = label[:61] + "..."
			}
			rows = append(rows, []models.InlineKeyboardButton{
				{Text: label, CallbackData: fmt.Sprintf("sel:%s:%s", inst.ID, sess.ID)},
			})
		}
	}

	if len(rows) == 0 {
		b.send(ctx, "No sessions found. Use /cc\\_new to create one.")
		return
	}

	b.bot.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: b.chatID,
		Text:   "Select a session:",
		ReplyMarkup: &models.InlineKeyboardMarkup{
			InlineKeyboard: rows,
		},
	})
}

// handleSelectCallback 处理 session 选择回调
func (b *Bot) handleSelectCallback(ctx context.Context, _ *bot.Bot, update *models.Update) {
	cb := update.CallbackQuery
	if cb.Message.Message != nil && cb.Message.Message.Chat.ID != b.chatID {
		return
	}

	b.bot.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
		CallbackQueryID: cb.ID,
	})

	// 格式: sel:{instanceID}:{sessionID}
	parts := strings.SplitN(cb.Data, ":", 3)
	if len(parts) < 3 {
		return
	}
	instanceID := parts[1]
	sessionID := parts[2]

	inst, err := b.store.Get(instanceID)
	if err != nil {
		b.send(ctx, "❌ Instance not found")
		return
	}

	b.ClearActive()
	b.SetActive(inst.ID, sessionID)

	sessID := sessionID
	if len(sessID) > 8 {
		sessID = sessID[:8]
	}
	b.send(ctx, fmt.Sprintf("✅ Session `%s` activated on *%s*\n\nSend a message to continue chatting.", sessID, inst.Name))

	b.streams.EnsureStream(inst, sessionID)
}

// handleList 列出所有 session
func (b *Bot) handleList(ctx context.Context, msg *models.Message) {
	running := b.runningInstances()
	if len(running) == 0 {
		b.send(ctx, "No instances found")
		return
	}

	active := b.GetActive()

	var sb strings.Builder
	sb.WriteString("*Sessions*\n\n")
	total := 0

	for _, inst := range running {
		sessions, err := b.listOpencodeSessions(ctx, inst)
		if err != nil || len(sessions) == 0 {
			continue
		}
		sb.WriteString(fmt.Sprintf("📦 *%s* (%s)\n", inst.Name, inst.Status))
		for _, sess := range sessions {
			total++
			title := sess.Title
			if title == "" {
				title = "(untitled)"
			}
			marker := ""
			if active != nil && active.sessionID == sess.ID {
				marker = " ← active"
			}
			sessID := sess.ID
			if len(sessID) > 8 {
				sessID = sessID[:8]
			}
			sb.WriteString(fmt.Sprintf("  `%s` %s%s\n", sessID, title, marker))
		}
		sb.WriteString("\n")
	}

	if total == 0 {
		b.send(ctx, "No sessions found")
		return
	}

	b.send(ctx, sb.String())
}

// handleAbort 中止活跃 session
func (b *Bot) handleAbort(ctx context.Context, msg *models.Message) {
	active := b.GetActive()
	if active == nil {
		b.send(ctx, "❌ No active session")
		return
	}

	inst, err := b.store.Get(active.instanceID)
	if err != nil {
		b.send(ctx, "❌ Instance not found")
		return
	}

	url := opencodeURL(inst.ID, inst.Port, "/session/"+active.sessionID+"/abort")
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		b.send(ctx, fmt.Sprintf("❌ Abort failed: %v", err))
		return
	}
	resp.Body.Close()

	b.send(ctx, "✅ Session aborted")
}

// handleExit 断开活跃 session（停止 SSE，清除 active）
func (b *Bot) handleExit(ctx context.Context, msg *models.Message) {
	active := b.GetActive()
	if active == nil {
		b.send(ctx, "No active session")
		return
	}
	b.ClearActive()
	b.send(ctx, "✅ Disconnected from session")
}

// handleSessionMessage 转发消息到活跃 session
func (b *Bot) handleSessionMessage(ctx context.Context, msg *models.Message) {
	active := b.GetActive()
	if active == nil {
		b.send(ctx, "No active session. Use /cc\\_new or /cc\\_select")
		return
	}

	inst, err := b.store.Get(active.instanceID)
	if err != nil {
		b.send(ctx, "❌ Instance not found")
		return
	}

	log.Printf("[telegram] forwarding message to session %s on instance %s (port %d)", active.sessionID[:8], inst.ID, inst.Port)

	// 确保 SSE 流在运行
	b.streams.EnsureStream(inst, active.sessionID)

	body := opencodeMessageBody{
		Parts: []opencodeMessagePart{
			{Type: "text", Text: msg.Text},
		},
	}
	bodyJSON, _ := json.Marshal(body)

	// opencode API 端点: /session/{id}/prompt_async，返回 204 表示接受
	url := opencodeURL(inst.ID, inst.Port, "/session/"+active.sessionID+"/prompt_async")
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyJSON))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		b.send(ctx, fmt.Sprintf("❌ Failed to send message: %v", err))
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		log.Printf("[telegram] prompt_async rejected: status=%d body=%s", resp.StatusCode, string(respBody))
		b.send(ctx, fmt.Sprintf("❌ opencode rejected prompt (status %d): %s", resp.StatusCode, string(respBody)))
		return
	}
	log.Printf("[telegram] prompt_async accepted for session %s (status %d)", active.sessionID[:8], resp.StatusCode)
}

// handlePermissionCallback 处理权限回调
func (b *Bot) handlePermissionCallback(ctx context.Context, _ *bot.Bot, update *models.Update) {
	cb := update.CallbackQuery

	b.bot.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
		CallbackQueryID: cb.ID,
	})

	// 格式: perm:{action}:{instanceID}:{sessionID}:{permissionID}
	parts := strings.SplitN(cb.Data, ":", 5)
	if len(parts) < 5 {
		return
	}
	action := parts[1]
	instanceID := parts[2]
	sessionID := parts[3]
	permissionID := parts[4]

	inst, err := b.store.Get(instanceID)
	if err != nil {
		return
	}

	// opencode 权限响应格式: {"response": "once"|"always"|"reject"}
	response := "once"
	if action != "allow" {
		response = "reject"
	}
	permBody := map[string]string{"response": response}
	bodyJSON, _ := json.Marshal(permBody)

	// 端点: POST /session/{id}/permissions/{permissionID}
	url := opencodeURL(inst.ID, inst.Port, "/session/"+sessionID+"/permissions/"+permissionID)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyJSON))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("[telegram] permission response failed: %v", err)
		return
	}
	resp.Body.Close()

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

// --- Helpers ---

func (b *Bot) runningInstances() []*store.Instance {
	instances := b.getInstances()
	var running []*store.Instance
	for _, inst := range instances {
		if inst.Status == "running" {
			running = append(running, inst)
		}
	}
	return running
}

func (b *Bot) listOpencodeSessions(ctx context.Context, inst *store.Instance) ([]opencodeSession, error) {
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

func (b *Bot) createOpencodeSession(ctx context.Context, inst *store.Instance) (*opencodeSession, error) {
	bodyJSON, _ := json.Marshal(map[string]string{})

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
