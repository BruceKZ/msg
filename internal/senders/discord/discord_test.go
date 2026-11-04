package discord

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"msg/internal/core"
	"msg/internal/senders"
)

func TestValidWebhook(t *testing.T) {
	for u, want := range map[string]bool{
		"https://discord.com/api/webhooks/1/abc":    true,
		"https://discordapp.com/api/webhooks/1/abc": true,
		"http://discord.com/api/webhooks/1/abc":     false,
		"https://evil.com/api/webhooks/1/abc":       false,
		"https://discord.com/other":                 false,
	} {
		if validWebhook(u) != want {
			t.Errorf("%s: want %v", u, want)
		}
	}
}

func TestSend(t *testing.T) {
	var got map[string]any
	status := 204
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(status)
	}))
	defer srv.Close()
	AllowAnyHost = true
	defer func() { AllowAnyHost = false }()

	env := senders.Env{Loc: time.UTC, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s, err := Type().Build("dc", senders.Config{"webhook_url": srv.URL + "/api/webhooks/1/secret", "username": "msg"}, env)
	if err != nil {
		t.Fatal(err)
	}
	e := core.Envelope{Kind: "sms", Title: "95588", Body: "hello @everyone", Fields: []core.Field{{Name: "来源", Value: "iphone"}},
		Time: time.Now(), Priority: core.PriorityHigh}
	if err := s.Send(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	embed := got["embeds"].([]any)[0].(map[string]any)
	if embed["title"] != "95588" || embed["color"].(float64) != 0xED4245 || got["username"] != "msg" {
		t.Fatalf("payload: %v", got)
	}
	if len(got["allowed_mentions"].(map[string]any)["parse"].([]any)) != 0 {
		t.Fatal("mentions must be disabled")
	}

	status = 404
	err = s.Send(context.Background(), e)
	if _, ok := err.(core.Permanent); !ok {
		t.Fatalf("404 should be permanent: %v", err)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatal("webhook URL leaked")
	}
	status = 429
	if err := s.Send(context.Background(), e); err == nil {
		t.Fatal("429 should fail")
	} else if _, ok := err.(core.Permanent); ok {
		t.Fatal("429 should be retried")
	}
}
