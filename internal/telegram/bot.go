package telegram

import (
	"context"
	"log"
	"strings"
	"sync"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/naiba/cloudcode/internal/store"
)

// activeSession 当前对接的 opencode session
type activeSession struct {
	instanceID string
	sessionID  string
}

// Bot wraps a Telegram bot that bridges chat messages to opencode sessions.
// 单窗口模式：所有消息在默认聊天窗口，通过命令切换活跃 session。
type Bot struct {
	bot   *bot.Bot
	store *store.Store

	chatID  int64 // 只有此 chat 有权交互
	streams *StreamManager

	mu     sync.Mutex
	active *activeSession // 当前对接的 session，nil 表示未选择

	getInstances func() []*store.Instance
}

// New creates and configures a Telegram bot.
func New(ctx context.Context, botToken string, chatID int64, s *store.Store, getInstances func() []*store.Instance) (*Bot, error) {
	b := &Bot{
		store:        s,
		chatID:       chatID,
		getInstances: getInstances,
	}

	b.streams = NewStreamManager(s)

	tgBot, err := bot.New(botToken,
		bot.WithDefaultHandler(b.defaultHandler),
		bot.WithCallbackQueryDataHandler("new:", bot.MatchTypePrefix, b.handleNewCallback),
		bot.WithCallbackQueryDataHandler("sel:", bot.MatchTypePrefix, b.handleSelectCallback),
		bot.WithCallbackQueryDataHandler("perm:", bot.MatchTypePrefix, b.handlePermissionCallback),
	)
	if err != nil {
		return nil, err
	}
	b.bot = tgBot
	b.streams.bot = tgBot
	b.streams.chatID = chatID

	// 注册命令列表到 Telegram，让客户端自动展示命令菜单
	b.registerCommands(ctx)

	return b, nil
}

// Start begins long polling. Blocks until ctx is cancelled.
func (b *Bot) Start(ctx context.Context) {
	log.Printf("[telegram] bot starting, chatID=%d", b.chatID)
	b.bot.Start(ctx)
}

// SetActive 设置当前活跃 session
func (b *Bot) SetActive(instanceID, sessionID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.active = &activeSession{instanceID: instanceID, sessionID: sessionID}
}

// ClearActive 清除活跃 session 并停止 SSE
func (b *Bot) ClearActive() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.active != nil {
		b.streams.Stop(b.active.sessionID)
		b.active = nil
	}
}

// GetActive 返回当前活跃 session（可能为 nil）
func (b *Bot) GetActive() *activeSession {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.active == nil {
		return nil
	}
	cp := *b.active
	return &cp
}

// registerCommands 通过 SetMyCommands API 推送命令列表到 Telegram
func (b *Bot) registerCommands(ctx context.Context) {
	_, err := b.bot.SetMyCommands(ctx, &bot.SetMyCommandsParams{
		Commands: []models.BotCommand{
			{Command: "cc_new", Description: "Create a new session"},
			{Command: "cc_select", Description: "Select an existing session"},
			{Command: "cc_list", Description: "List all sessions"},
			{Command: "cc_abort", Description: "Abort current session"},
			{Command: "cc_exit", Description: "Disconnect from session"},
			{Command: "start", Description: "Show help"},
		},
	})
	if err != nil {
		log.Printf("[telegram] failed to register commands: %v", err)
	}
}

// defaultHandler routes all incoming messages.
func (b *Bot) defaultHandler(ctx context.Context, _ *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}

	msg := update.Message

	// 鉴权：只响应配置的 chat
	if msg.Chat.ID != b.chatID {
		return
	}

	// 路由命令（支持 /cc_xxx 和 /cc-xxx 两种格式，以及 @botname 后缀）
	if msg.Text != "" && strings.HasPrefix(msg.Text, "/") {
		cmd := strings.SplitN(msg.Text, " ", 2)[0]
		if i := strings.Index(cmd, "@"); i > 0 {
			cmd = cmd[:i]
		}
		// 统一 - 和 _ 的处理
		cmd = strings.ReplaceAll(cmd, "-", "_")
		switch cmd {
		case "/cc_new":
			b.handleNew(ctx, msg)
			return
		case "/cc_select":
			b.handleSelect(ctx, msg)
			return
		case "/cc_list":
			b.handleList(ctx, msg)
			return
		case "/cc_abort":
			b.handleAbort(ctx, msg)
			return
		case "/cc_exit":
			b.handleExit(ctx, msg)
			return
		case "/start", "/cc_help":
			b.handleHelp(ctx, msg)
			return
		}
	}

	// 非命令消息 → 转发到活跃 session
	if msg.Text != "" {
		b.handleSessionMessage(ctx, msg)
	}
}

// send 发送消息到默认窗口
func (b *Bot) send(ctx context.Context, text string) {
	b.bot.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:    b.chatID,
		Text:      text,
		ParseMode: models.ParseModeMarkdown,
	})
}
