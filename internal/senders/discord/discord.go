// Package discord is a sender that posts envelopes to a Discord channel webhook.
package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"msg/internal/core"
	"msg/internal/senders"
)

func Type() senders.Type {
	return senders.Type{
		Name:        "discord",
		Label:       "Discord（Webhook）",
		Description: "推送到一个 Discord 频道。在频道设置 → 整合 → Webhooks 新建并复制 URL。",
		Fields: []senders.Field{
			{Name: "webhook_url", Label: "Webhook URL", Kind: senders.FieldSecret, Required: true,
				Help: "形如 https://discord.com/api/webhooks/…"},
			{Name: "username", Label: "显示名称", Kind: senders.FieldString,
				Help: "可选，覆盖 webhook 默认的名字。"},
		},
		Build: func(name string, cfg senders.Config, env senders.Env) (core.Sender, error) {
			u := cfg.String("webhook_url")
			if !validWebhook(u) {
				return nil, &core.ValidationError{Field: "config.webhook_url", Message: "must be a https://discord.com/api/webhooks/… URL"}
			}
			return &Webhook{name: name, url: u, username: cfg.String("username"), loc: env.Loc,
				http: &http.Client{Timeout: 20 * time.Second}}, nil
		},
	}
}

// AllowAnyHost lets tests point the webhook at a local server.
var AllowAnyHost = false

func validWebhook(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if AllowAnyHost {
		return u.Scheme == "http" || u.Scheme == "https"
	}
	switch u.Host {
	case "discord.com", "discordapp.com", "ptb.discord.com", "canary.discord.com":
	default:
		return false
	}
	return u.Scheme == "https" && strings.HasPrefix(u.Path, "/api/webhooks/")
}

type Webhook struct {
	name, url, username string
	loc                 *time.Location
	http                *http.Client
}

func (w *Webhook) Name() string { return w.name }

func trunc(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func (w *Webhook) payload(e core.Envelope) map[string]any {
	color := 0x5865F2 // Discord blurple
	if e.Priority == core.PriorityHigh {
		color = 0xED4245
	}
	fields := []map[string]any{}
	for i, f := range e.Fields {
		if i == 25 {
			break
		}
		fields = append(fields, map[string]any{"name": trunc(f.Name, 256), "value": trunc(f.Value, 1024), "inline": true})
	}
	embed := map[string]any{
		"title":       trunc(e.Title, 256),
		"description": trunc(e.Body, 4096),
		"color":       color,
		"fields":      fields,
		"footer":      map[string]string{"text": e.Kind},
		"timestamp":   e.Time.UTC().Format(time.RFC3339),
	}
	if e.Link != "" && (strings.HasPrefix(e.Link, "https://") || strings.HasPrefix(e.Link, "http://")) {
		embed["url"] = e.Link
	}
	p := map[string]any{
		"embeds":           []any{embed},
		"allowed_mentions": map[string]any{"parse": []string{}}, // never ping from message content
	}
	if w.username != "" {
		p["username"] = trunc(w.username, 80)
	}
	return p
}

func (w *Webhook) Send(ctx context.Context, e core.Envelope) error {
	body, _ := json.Marshal(w.payload(e))
	req, err := http.NewRequestWithContext(ctx, "POST", w.url, bytes.NewReader(body))
	if err != nil {
		return core.Permanent{Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := w.http.Do(req)
	if err != nil {
		// The webhook URL is a secret; keep it out of stored errors.
		return fmt.Errorf("discord: %s", strings.ReplaceAll(err.Error(), w.url, "<webhook>"))
	}
	defer res.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
	switch {
	case res.StatusCode >= 200 && res.StatusCode < 300:
		return nil
	case res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500:
		if ra := res.Header.Get("Retry-After"); ra != "" {
			if s, err := strconv.ParseFloat(ra, 64); err == nil {
				return fmt.Errorf("discord: http %d, retry after %.0fs", res.StatusCode, s)
			}
		}
		return fmt.Errorf("discord: http %d", res.StatusCode)
	default:
		return core.Permanent{Err: fmt.Errorf("discord: http %d %s", res.StatusCode, strings.TrimSpace(string(msg)))}
	}
}
