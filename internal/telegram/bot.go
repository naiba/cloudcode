package telegram

import (
	"context"
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
		}
	}

	// Regular message in a session topic → forward to opencode
	if msg.MessageThreadID != 0 {
		b.handler.handleSessionMessage(ctx, msg)
	}
}
