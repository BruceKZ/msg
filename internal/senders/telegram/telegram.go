// Package telegram is a sender that delivers envelopes through a Telegram bot,
// plus a small set of kind-agnostic chat commands for browsing messages.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"msg/internal/core"
	"msg/internal/senders"
	"msg/internal/store"
)

// Type registers Telegram as a sender type that can be configured at runtime.
func Type() senders.Type {
	return senders.Type{
		Name:        "telegram",
		Label:       "Telegram",
		Description: "通过 Telegram bot 推送；在 @BotFather 创建 bot 获取 token。",
		Fields: []senders.Field{
			{Name: "bot_token", Label: "Bot token", Kind: senders.FieldSecret, Required: true,
				Help: "@BotFather 给的 token，形如 123456789:AA…"},
			{Name: "chat_ids", Label: "Chat IDs", Kind: senders.FieldIntList,
				Help: "接收推送的会话。先给 bot 发一句话，再从“最近的会话”里添加。"},
			{Name: "commands", Label: "启用聊天命令", Kind: senders.FieldBool,
				Help: "允许在上面这些会话里用 /latest、/recent 查看消息。"},
		},
		Build: func(name string, cfg senders.Config, env senders.Env) (core.Sender, error) {
			tok := cfg.String("bot_token")
			if !tokenRe.MatchString(tok) {
				return nil, &core.ValidationError{Field: "config.bot_token", Message: "does not look like a Telegram bot token"}
			}
			b, err := New(Config{Name: name, BotToken: tok, ChatIDs: cfg.Ints("chat_ids"), Commands: cfg.Bool("commands"), APIBase: APIBase},
				env.Loc, env.Log.With("sender", name))
			if err != nil {
				return nil, err
			}
			b.EnableCommands(env.Inbox, env.KindNames)
			return b, nil
		},
	}
}

var tokenRe = regexp.MustCompile(`^[0-9]{5,}:[A-Za-z0-9_-]{30,}$`)

// APIBase is the Telegram Bot API endpoint (overridable in tests).
var APIBase = "https://api.telegram.org"

type Config struct {
	Name     string  `json:"name"`
	BotToken string  `json:"bot_token"`
	ChatIDs  []int64 `json:"chat_ids"`
	Commands bool    `json:"commands"`
	APIBase  string  `json:"api_base"` // default https://api.telegram.org
}

type Bot struct {
	cfg       Config
	loc       *time.Location
	http      *http.Client
	log       *slog.Logger
	inbox     senders.Inbox
	kindNames func() []string

	mu    sync.Mutex
	chats map[int64]senders.Chat
}

func New(cfg Config, loc *time.Location, log *slog.Logger) (*Bot, error) {
	if cfg.Name == "" {
		return nil, fmt.Errorf("telegram: name is required")
	}
	if cfg.BotToken == "" {
		return nil, fmt.Errorf("telegram %s: bot_token is required", cfg.Name)
	}
	if cfg.APIBase == "" {
		cfg.APIBase = "https://api.telegram.org"
	}
	return &Bot{cfg: cfg, loc: loc, http: &http.Client{Timeout: 70 * time.Second}, log: log, chats: map[int64]senders.Chat{}}, nil
}

// EnableCommands gives the bot read access for its chat commands.
func (b *Bot) EnableCommands(inbox senders.Inbox, kindNames func() []string) {
	b.inbox, b.kindNames = inbox, kindNames
}

func (b *Bot) Name() string { return b.cfg.Name }

func (b *Bot) Send(ctx context.Context, e core.Envelope) error {
	if len(b.cfg.ChatIDs) == 0 {
		return core.Permanent{Err: fmt.Errorf("telegram %s: no chat_ids configured", b.cfg.Name)}
	}
	text := b.Format(e, 0)
	var firstErr error
	for _, id := range b.cfg.ChatIDs {
		if err := b.sendText(ctx, id, text); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Format renders an envelope as Telegram HTML. id > 0 adds the message id.
func (b *Bot) Format(e core.Envelope, id int64) string {
	var sb strings.Builder
	if e.Priority == core.PriorityHigh {
		sb.WriteString("❗ ")
	}
	if e.Title != "" {
		fmt.Fprintf(&sb, "<b>%s</b>\n", esc(e.Title))
	}
	body := e.Body
	if r := []rune(body); len(r) > 3500 { // Telegram caps a message at 4096 characters
		body = string(r[:3500]) + "…"
	}
	if body != "" {
		sb.WriteString(esc(body))
		sb.WriteString("\n")
	}
	if len(e.Fields) > 0 {
		sb.WriteString("\n")
		for _, f := range e.Fields {
			fmt.Fprintf(&sb, "%s: %s\n", esc(f.Name), esc(f.Value))
		}
	}
	if e.Link != "" {
		fmt.Fprintf(&sb, "\n<a href=\"%s\">详情</a>\n", esc(e.Link))
	}
	meta := []string{esc(e.Kind), e.Time.In(b.loc).Format("01-02 15:04:05")}
	if id > 0 {
		meta = append(meta, "#"+strconv.FormatInt(id, 10))
	}
	fmt.Fprintf(&sb, "\n<i>%s</i>", strings.Join(meta, " · "))
	return strings.TrimLeft(sb.String(), "\n")
}

func esc(s string) string { return html.EscapeString(s) }

type apiResp struct {
	OK          bool            `json:"ok"`
	Description string          `json:"description"`
	ErrorCode   int             `json:"error_code"`
	Result      json.RawMessage `json:"result"`
}

func (b *Bot) call(ctx context.Context, method string, payload, out any) error {
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, "POST", b.cfg.APIBase+"/bot"+b.cfg.BotToken+"/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := b.http.Do(req)
	if err != nil {
		// The token is part of the URL; keep it out of logs and stored errors.
		return fmt.Errorf("telegram %s: %s", method, strings.ReplaceAll(err.Error(), b.cfg.BotToken, "***"))
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	var r apiResp
	if err := json.Unmarshal(raw, &r); err != nil {
		return fmt.Errorf("telegram %s: http %d", method, res.StatusCode)
	}
	if !r.OK {
		err := fmt.Errorf("telegram %s: %d %s", method, r.ErrorCode, r.Description)
		switch r.ErrorCode {
		case 400, 401, 403, 404:
			return core.Permanent{Err: err}
		}
		return err
	}
	if out != nil {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}

func (b *Bot) sendText(ctx context.Context, chatID int64, text string) error {
	return b.call(ctx, "sendMessage", map[string]any{
		"chat_id": chatID, "text": text, "parse_mode": "HTML", "disable_web_page_preview": true,
	}, nil)
}

// ---- chat commands ----

type update struct {
	UpdateID int64 `json:"update_id"`
	Message  *struct {
		Text string `json:"text"`
		Chat struct {
			ID        int64  `json:"id"`
			Type      string `json:"type"`
			Title     string `json:"title"`
			Username  string `json:"username"`
			FirstName string `json:"first_name"`
			LastName  string `json:"last_name"`
		} `json:"chat"`
	} `json:"message"`
}

var commands = []map[string]string{
	{"command": "latest", "description": "最新一条消息，可加类型：/latest sms"},
	{"command": "recent", "description": "最近 N 条（默认 5），可加类型：/recent 10 sms"},
	{"command": "kinds", "description": "列出消息类型"},
	{"command": "id", "description": "显示当前 chat id"},
	{"command": "help", "description": "帮助"},
}

// Run long-polls the bot's updates until ctx is done. It always records which
// chats talk to the bot (so they can be picked in the settings) and answers
// /id; the other commands need commands enabled and an allowed chat.
func (b *Bot) Run(ctx context.Context) {
	if b.cfg.Commands {
		if err := b.call(ctx, "setMyCommands", map[string]any{"commands": commands}, nil); err != nil && ctx.Err() == nil {
			b.log.Warn("telegram setMyCommands", "err", err)
		}
	}
	var offset int64
	for ctx.Err() == nil {
		var ups []update
		err := b.call(ctx, "getUpdates", map[string]any{"offset": offset, "timeout": 50, "allowed_updates": []string{"message"}}, &ups)
		if err != nil {
			if ctx.Err() == nil {
				b.log.Warn("telegram getUpdates", "err", err)
				wait := 10 * time.Second
				var perm core.Permanent
				if errors.As(err, &perm) {
					wait = 5 * time.Minute // e.g. revoked token: don't hammer the API
				}
				select {
				case <-ctx.Done():
				case <-time.After(wait):
				}
			}
			continue
		}
		for _, u := range ups {
			offset = u.UpdateID + 1
			if m := u.Message; m != nil {
				title := m.Chat.Title
				if title == "" {
					title = strings.TrimSpace(m.Chat.FirstName + " " + m.Chat.LastName)
				}
				if m.Chat.Username != "" {
					title = strings.TrimSpace(title + " @" + m.Chat.Username)
				}
				b.seen(senders.Chat{ID: m.Chat.ID, Title: title, Type: m.Chat.Type, LastSeen: time.Now().UTC()})
				b.handle(ctx, m.Chat.ID, m.Text)
			}
		}
	}
}

const maxChats = 20

func (b *Bot) seen(c senders.Chat) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.chats[c.ID] = c
	if len(b.chats) > maxChats {
		var oldest int64
		var t time.Time
		for id, ch := range b.chats {
			if t.IsZero() || ch.LastSeen.Before(t) {
				oldest, t = id, ch.LastSeen
			}
		}
		delete(b.chats, oldest)
	}
}

// RecentChats lists chats that messaged the bot, most recent first.
func (b *Bot) RecentChats() []senders.Chat {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]senders.Chat, 0, len(b.chats))
	for _, c := range b.chats {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeen.After(out[j].LastSeen) })
	return out
}

// SeedChats carries recent chats over when the sender is rebuilt.
func (b *Bot) SeedChats(cs []senders.Chat) {
	for _, c := range cs {
		b.seen(c)
	}
}

func (b *Bot) handle(ctx context.Context, chatID int64, text string) {
	args := strings.Fields(text)
	if len(args) == 0 || !strings.HasPrefix(args[0], "/") {
		return
	}
	cmd := strings.ToLower(strings.SplitN(args[0], "@", 2)[0])
	args = args[1:]
	reply := func(s string) {
		if err := b.sendText(ctx, chatID, s); err != nil {
			b.log.Warn("telegram reply", "err", err)
		}
	}

	if cmd == "/id" || cmd == "/start" {
		reply(fmt.Sprintf("chat id: <code>%d</code>", chatID))
		return
	}
	if !b.cfg.Commands || b.inbox == nil {
		return
	}
	if !slices.Contains(b.cfg.ChatIDs, chatID) {
		return // strangers only get /id
	}

	switch cmd {
	case "/help":
		var sb strings.Builder
		for _, c := range commands {
			fmt.Fprintf(&sb, "/%s — %s\n", c["command"], esc(c["description"]))
		}
		reply(sb.String())
	case "/kinds":
		reply(esc(strings.Join(b.kindNames(), "\n")))
	case "/latest", "/recent":
		n := 1
		if cmd == "/recent" {
			n = 5
		}
		var kinds []string
		for _, a := range args {
			if v, err := strconv.Atoi(a); err == nil && cmd == "/recent" {
				n = min(max(v, 1), 20)
			} else if slices.Contains(b.kindNames(), a) {
				kinds = append(kinds, a)
			} else {
				reply("未知类型：" + esc(a) + "\n可用类型：" + esc(strings.Join(b.kindNames(), ", ")))
				return
			}
		}
		ms, err := b.inbox.List(ctx, store.Filter{Kinds: kinds, Limit: n})
		if err != nil {
			reply("查询失败：" + esc(err.Error()))
			return
		}
		if len(ms) == 0 {
			reply("没有消息")
			return
		}
		for i := len(ms) - 1; i >= 0; i-- {
			reply(b.Format(ms[i].Envelope, ms[i].ID))
		}
	}
}
