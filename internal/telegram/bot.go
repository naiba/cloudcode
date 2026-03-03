package telegram

import (
	"context"
	"fmt"
	"log"
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

// setupWatchdogTopic creates or finds the "🐕 Prompt Watchdog" topic and pins it.
func (b *Bot) setupWatchdogTopic(ctx context.Context) {
	// Try to find existing watchdog topic by checking pinned message or stored state.
	// Since Telegram doesn't have a "list topics" API, we create it and handle duplicates.
	topic, err := b.bot.CreateForumTopic(ctx, &bot.CreateForumTopicParams{
		ChatID: b.chatID,
		Name:   watchdogTopicName,
	})
	if err != nil {
		// Topic might already exist or bot lacks permission — log and continue
		log.Printf("[telegram] failed to create watchdog topic: %v", err)
		return
	}

	b.watchdog = topic.MessageThreadID
	log.Printf("[telegram] watchdog topic created: threadID=%d", b.watchdog)
}

// defaultHandler routes all incoming updates.
func (b *Bot) defaultHandler(ctx context.Context, _ *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}

	msg := update.Message

	// Auth check: only respond to the configured chat
	if msg.Chat.ID != b.chatID {
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
		}
	}

	result := fmt.Sprintf("✅ Sync complete: %d topics removed, %d topics created", removedCount, createdCount)
	if sb.Len() > 0 {
		result += "\n\n" + sb.String()
	}
	return result
}
