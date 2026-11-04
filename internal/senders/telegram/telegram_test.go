package telegram

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
)

func TestFormatEscapes(t *testing.T) {
	b, _ := New(Config{Name: "tg", BotToken: "t"}, time.UTC, nil)
	out := b.Format(core.Envelope{
		Kind: "sms", Title: "<b>evil</b>", Body: "a & b < c",
		Fields:   []core.Field{{Name: "来源", Value: "iphone"}},
		Time:     time.Date(2026, 11, 4, 13, 0, 0, 0, time.UTC),
		Priority: core.PriorityHigh,
	}, 7)
	for _, want := range []string{"❗ ", "<b>&lt;b&gt;evil&lt;/b&gt;</b>", "a &amp; b &lt; c", "来源: iphone", "<i>sms · 11-04 13:00:00 · #7</i>"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestSend(t *testing.T) {
	var got []map[string]any
	status := 200
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/botSECRET/sendMessage") {
			t.Errorf("path %s", r.URL.Path)
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		got = append(got, body)
		w.WriteHeader(status)
		if status == 200 {
			io.WriteString(w, `{"ok":true,"result":{}}`)
		} else {
			io.WriteString(w, `{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`)
		}
	}))
	defer srv.Close()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	b, _ := New(Config{Name: "tg", BotToken: "SECRET", ChatIDs: []int64{1, 2}, APIBase: srv.URL}, time.UTC, log)
	if err := b.Send(context.Background(), core.Envelope{Title: "t", Body: "b"}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0]["parse_mode"] != "HTML" {
		t.Fatalf("requests: %v", got)
	}

	status = 400
	err := b.Send(context.Background(), core.Envelope{Title: "t"})
	var perm core.Permanent
	if err == nil || !asPermanent(err, &perm) {
		t.Fatalf("400 should be permanent: %v", err)
	}
	if strings.Contains(err.Error(), "SECRET") {
		t.Fatal("token leaked in error")
	}

	noChats, _ := New(Config{Name: "tg", BotToken: "SECRET"}, time.UTC, log)
	if err := noChats.Send(context.Background(), core.Envelope{}); !asPermanent(err, &perm) {
		t.Fatalf("no chat ids should be permanent: %v", err)
	}
}

func asPermanent(err error, p *core.Permanent) bool {
	pe, ok := err.(core.Permanent)
	if ok {
		*p = pe
	}
	return ok
}
