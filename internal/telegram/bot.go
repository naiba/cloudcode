package telegram

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/naiba/cloudcode/internal/store"
)

const watchdogTopicName = "🐕 Prompt Watchdog"

// Bot wraps a Telegram bot that bridges forum topics to opencode sessions.
type Bot struct {
	bot      *bot.Bot
	store    *store.Store
	chatID   int64 // only this chat is authorized to interact
	streams  *StreamManager
	handler  *Handler
	watchdog int // watchdog topic thread ID (0 = not yet created)

	getInstances func() []*store.Instance
}

// New creates and configures a Telegram bot.
// botToken and chatID come from CC_TELEGRAM_BOT_TOKEN / CC_TELEGRAM_CHAT_ID.
// getInstances returns running instances with port info for opencode API calls.
func New(ctx context.Context, botToken string, chatID int64, s *store.Store, getInstances func() []*store.Instance) (*Bot, error) {
	b := &Bot{
		store:        s,
		chatID:       chatID,
		getInstances: getInstances,
	}

	b.streams = NewStreamManager(s)
	b.handler = NewHandler(b)

	tgBot, err := bot.New(botToken,
		bot.WithDefaultHandler(b.defaultHandler),
		bot.WithCallbackQueryDataHandler("new:", bot.MatchTypePrefix, b.handler.handleNewCallback),
		bot.WithCallbackQueryDataHandler("perm:", bot.MatchTypePrefix, b.handler.handlePermissionCallback),
	)
	if err != nil {
		return nil, err
	}
	b.bot = tgBot

	// Assign bot reference to stream manager after creation
	b.streams.bot = tgBot
	b.streams.chatID = chatID

	b.setupWatchdogTopic(ctx)
	return b, nil
}

// Start begins long polling in a goroutine. Blocks until ctx is cancelled.
func (b *Bot) Start(ctx context.Context) {
	log.Printf("[telegram] bot starting long polling, chatID=%d", b.chatID)
	b.bot.Start(ctx)
}

// WatchdogTopicID returns the thread ID of the pinned "Prompt Watchdog" topic.
// Returns 0 if not yet created.
func (b *Bot) WatchdogTopicID() int {
	return b.watchdog
}

// setupWatchdogTopic ensures a single "🐕 Prompt Watchdog" topic exists.
// 先从 DB 读取已持久化的 threadID，避免每次启动重复创建 topic。
func (b *Bot) setupWatchdogTopic(ctx context.Context) {
	// 从 DB 恢复已有的 watchdog topic ID
	if saved := b.store.GetSetting("watchdog_topic_id"); saved != "" {
		if id, err := strconv.Atoi(saved); err == nil && id > 0 {
			b.watchdog = id
			log.Printf("[telegram] watchdog topic restored from DB: threadID=%d", id)
			return
		}
	}

	// DB 中没有 → 创建新 topic
	topic, err := b.bot.CreateForumTopic(ctx, &bot.CreateForumTopicParams{
		ChatID: b.chatID,
		Name:   watchdogTopicName,
	})
	if err != nil {
		log.Printf("[telegram] failed to create watchdog topic: %v", err)
		return
	}

	b.watchdog = topic.MessageThreadID
	log.Printf("[telegram] watchdog topic created: threadID=%d", b.watchdog)

	// 持久化到 DB，下次启动不再重复创建
	if err := b.store.SetSetting("watchdog_topic_id", strconv.Itoa(topic.MessageThreadID)); err != nil {
		log.Printf("[telegram] failed to persist watchdog topic ID: %v", err)
	}
}

// defaultHandler routes all incoming updates.
func (b *Bot) defaultHandler(ctx context.Context, _ *bot.Bot, update *models.Update) {
	if update.Message == nil {
		log.Printf("[telegram] update without Message: %+v", update)
		return
	}

	msg := update.Message
	log.Printf("[telegram] message: chat=%d thread=%d text=%q", msg.Chat.ID, msg.MessageThreadID, msg.Text)

	// Auth check: only respond to the configured chat
	if msg.Chat.ID != b.chatID {
		log.Printf("[telegram] ignoring message from chat %d (authorized: %d)", msg.Chat.ID, b.chatID)
		return
	}

	// Route commands
	if msg.Text != "" && strings.HasPrefix(msg.Text, "/") {
		cmd := strings.SplitN(msg.Text, " ", 2)[0]
		// Strip @botname suffix from command (e.g. /new@mybot → /new)
		if i := strings.Index(cmd, "@"); i > 0 {
			cmd = cmd[:i]
		}
		switch cmd {
		case "/new":
			b.handler.handleNew(ctx, msg)
			return
		case "/abort":
			b.handler.handleAbort(ctx, msg)
			return
		case "/list":
			b.handler.handleList(ctx, msg)
			return
		case "/help", "/start":
			b.handler.handleHelp(ctx, msg)
			return
		case "/sync":
			b.handler.handleSync(ctx, msg)
			return
		}
	}

	// Regular message in a session topic → forward to opencode
	if msg.MessageThreadID != 0 {
		b.handler.handleSessionMessage(ctx, msg)
	}
}

// SyncTopics synchronizes Telegram topics with opencode sessions across all running instances.
// Returns a human-readable result string.
func (b *Bot) SyncTopics(ctx context.Context) string {
	instances := b.getInstances()
	var running []*store.Instance
	for _, inst := range instances {
		if inst.Status == "running" {
			running = append(running, inst)
		}
	}

	if len(running) == 0 {
		return "❌ No running instances"
	}

	var sb strings.Builder
	var removedCount, createdCount int

	for _, inst := range running {
		activeSessions, err := b.handler.listOpencodeSessions(ctx, inst)
		if err != nil {
			sb.WriteString(fmt.Sprintf("⚠️ %s: failed to list sessions: %v\n", inst.Name, err))
			continue
		}

		activeSet := make(map[string]*opencodeSession, len(activeSessions))
		for i := range activeSessions {
			activeSet[activeSessions[i].ID] = &activeSessions[i]
		}

		// Remove topics for deleted sessions
		topicSessions, _ := b.store.ListTopicSessionsByInstance(inst.ID)
		for _, ts := range topicSessions {
			if ts.SessionID == "" {
				continue
			}
			if _, exists := activeSet[ts.SessionID]; !exists {
				b.bot.DeleteForumTopic(ctx, &bot.DeleteForumTopicParams{
					ChatID:          b.chatID,
					MessageThreadID: ts.TopicID,
				})
				_ = b.store.DeleteTopicSession(ts.TopicID)
				removedCount++
			}
		}

		// Create topics for sessions without one
		mappedSessions := make(map[string]bool)
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
			topicTitle := sess.Title
			if topicTitle == "" {
				if len(sessID) >= 8 {
					topicTitle = sessID[:8]
				} else {
					topicTitle = sessID
				}
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

			// 创建 topic 后必须发一条消息，否则 Telegram 不会在聊天列表中显示该 topic
			b.bot.SendMessage(ctx, &bot.SendMessageParams{
				ChatID:          b.chatID,
				MessageThreadID: topic.MessageThreadID,
				Text:            fmt.Sprintf("Session `%s` linked to this topic.\nSend a message to start chatting.", sessID[:8]),
				ParseMode:       models.ParseModeMarkdown,
			})
		}
	}

	result := fmt.Sprintf("✅ Sync complete: %d topics removed, %d topics created", removedCount, createdCount)
	if sb.Len() > 0 {
		result += "\n\n" + sb.String()
	}

	// 在 General topic 发送同步结果，方便通过 Web UI 触发时也能在 Telegram 里看到
	b.bot.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: b.chatID,
		Text:   result,
	})

	return result
}
