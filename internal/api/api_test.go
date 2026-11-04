package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"msg/internal/auth"
	"msg/internal/core"
	"msg/internal/dispatch"
	"msg/internal/hub"
	"msg/internal/kinds/sms"
	"msg/internal/senders"
	"msg/internal/store"
	"msg/web"
)

// note is a second kind, to prove the API is not tied to sms.
type noteKind struct{}
type note struct {
	Text string `json:"text"`
}

func (noteKind) Info() core.KindInfo { return core.KindInfo{Name: "note", Description: "test"} }
func (noteKind) Decode(d json.RawMessage, _ core.Meta) (core.Message, error) {
	var n note
	if err := json.Unmarshal(d, &n); err != nil || n.Text == "" {
		return nil, &core.ValidationError{Field: "data.text", Message: "is required"}
	}
	return n, nil
}
func (n note) Compose(core.Meta) core.Envelope {
	return core.Envelope{Title: "note", Body: n.Text, Priority: core.PriorityHigh}
}

// fakeSender records what it sent; it fails the first `failures` sends.
type fakeSender struct {
	name     string
	mu       sync.Mutex
	sent     []core.Envelope
	failures int
	perm     bool
}

func (f *fakeSender) Name() string { return f.name }
func (f *fakeSender) Send(_ context.Context, e core.Envelope) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.perm {
		return core.Permanent{Err: errors.New("bad config")}
	}
	if f.failures > 0 {
		f.failures--
		return errors.New("temporary")
	}
	f.sent = append(f.sent, e)
	return nil
}
func (f *fakeSender) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.sent) }
func (f *fakeSender) set(failures int, perm bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures, f.perm = failures, perm
}
func (f *fakeSender) first() core.Envelope { f.mu.Lock(); defer f.mu.Unlock(); return f.sent[0] }

const (
	tokPhone  = "phone-token-0123456789"
	tokReader = "reader-token-0123456789"
	tokAdmin  = "admin-token-0123456789"
	tokNotes  = "notes-token-0123456789"
)

type env struct {
	t      *testing.T
	url    string
	tg, dc *fakeSender
}

func setup(t *testing.T) *env {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	tg, dc := &fakeSender{name: "tg"}, &fakeSender{name: "dc"}
	fakes := map[string]*fakeSender{"tg": tg, "dc": dc}
	fakeType := senders.Type{
		Name: "fake", Label: "Fake",
		Fields: []senders.Field{
			{Name: "secret", Label: "Secret", Kind: senders.FieldSecret},
			{Name: "note", Label: "Note", Kind: senders.FieldString},
		},
		Build: func(name string, cfg senders.Config, _ senders.Env) (core.Sender, error) {
			if cfg.String("note") == "bad" {
				return nil, &core.ValidationError{Field: "config.note", Message: "bad note"}
			}
			if f, ok := fakes[name]; ok {
				return f, nil
			}
			return &fakeSender{name: name}, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mgr, err := senders.New(ctx, st, []senders.Type{fakeType}, senders.Env{Loc: time.UTC, Log: log, Inbox: st, KindNames: func() []string { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"tg", "dc"} {
		if _, err := mgr.Create(ctx, senders.Change{Name: n, Type: "fake", Config: json.RawMessage(`{"secret":"s3cret"}`)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := mgr.SetRoute(ctx, "sms", []string{"tg"}); err != nil {
		t.Fatal(err)
	}

	d := dispatch.New(st, mgr, log)
	d.Backoff = []time.Duration{10 * time.Millisecond, 10 * time.Millisecond}
	d.PollEvery = 10 * time.Millisecond
	go d.Run(ctx)

	h, err := hub.New(st, []core.Kind{sms.New(), noteKind{}}, mgr, hub.Options{Wake: d.Wake})
	if err != nil {
		t.Fatal(err)
	}
	a, err := auth.New([]auth.Token{
		{Name: "iphone-backup", Token: tokPhone, Scopes: []string{"messages:write"}, Kinds: []string{"sms"}},
		{Name: "reader", Token: tokReader, Scopes: []string{"messages:read"}},
		{Name: "admin", Token: tokAdmin, Scopes: auth.AllScopes},
		{Name: "notes-only", Token: tokNotes, Scopes: []string{"messages:read", "messages:write"}, Kinds: []string{"note"}},
	}, nil, nil, h.KindNames())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(h, st, a, mgr, web.FS(), log).Handler())
	t.Cleanup(srv.Close)
	return &env{t: t, url: srv.URL, tg: tg, dc: dc}
}

type resp struct {
	status int
	header http.Header
	body   map[string]any
}

func (e *env) do(method, path, token, body string) resp {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.url+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	json.NewDecoder(res.Body).Decode(&out)
	return resp{res.StatusCode, res.Header, out}
}

func errCode(r resp) string {
	if e, ok := r.body["error"].(map[string]any); ok {
		return e["code"].(string)
	}
	return ""
}

func items(r resp) []map[string]any {
	var out []map[string]any
	for _, it := range r.body["items"].([]any) {
		out = append(out, it.(map[string]any))
	}
	return out
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestCreateAndRead(t *testing.T) {
	e := setup(t)
	r := e.do("POST", "/api/v1/messages", tokPhone, `{"kind":"sms","data":{"from":"95588","body":"hello"}}`)
	if r.status != 201 || r.header.Get("Location") != "/api/v1/messages/1" {
		t.Fatalf("create: %d %v %v", r.status, r.header.Get("Location"), r.body)
	}
	if r.body["source"] != "iphone-backup" || r.body["kind"] != "sms" {
		t.Fatalf("message: %v", r.body)
	}
	envl := r.body["envelope"].(map[string]any)
	if envl["title"] != "95588" || envl["body"] != "hello" || envl["priority"] != "normal" {
		t.Fatalf("envelope: %v", envl)
	}
	// Routed to tg only (routes), not dc.
	waitFor(t, func() bool { return e.tg.count() == 1 })
	time.Sleep(30 * time.Millisecond)
	if e.dc.count() != 0 {
		t.Fatal("sms should not reach dc")
	}

	g := e.do("GET", "/api/v1/messages/1", tokReader, "")
	if g.status != 200 || g.body["deliveries"].(map[string]any)["tg"] != "sent" {
		t.Fatalf("get: %d %v", g.status, g.body)
	}
	l := e.do("GET", "/api/v1/messages?kind=sms", tokReader, "")
	if l.status != 200 || len(items(l)) != 1 || l.body["next_before"] != nil {
		t.Fatalf("list: %v", l.body)
	}
}

func TestConcurrentDuplicates(t *testing.T) {
	e := setup(t)
	body := `{"kind":"sms","data":{"from":"淘宝","body":"验证码 482913"}}`
	var wg sync.WaitGroup
	var mu sync.Mutex
	created, dups := 0, 0
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := e.do("POST", "/api/v1/messages", tokPhone, body)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case r.status == 201:
				created++
			case r.status == 200 && r.header.Get("X-Duplicate") == "true":
				dups++
			default:
				t.Errorf("status %d %v", r.status, r.body)
			}
		}()
	}
	wg.Wait()
	if created != 1 || dups != 5 {
		t.Fatalf("created=%d dups=%d", created, dups)
	}
	waitFor(t, func() bool { return e.tg.count() == 1 })
	time.Sleep(30 * time.Millisecond)
	if e.tg.count() != 1 {
		t.Fatalf("sent %d times", e.tg.count())
	}
	// An explicit dedup_key overrides the kind's key.
	r := e.do("POST", "/api/v1/messages", tokPhone, `{"kind":"sms","data":{"from":"淘宝","body":"验证码 482913"},"dedup_key":"other"}`)
	if r.status != 201 {
		t.Fatalf("dedup_key: %d", r.status)
	}
}

func TestSendersOverrideAndRouting(t *testing.T) {
	e := setup(t)
	// No route for "note": goes to every sender.
	r := e.do("POST", "/api/v1/messages", tokNotes, `{"kind":"note","data":{"text":"exp done"}}`)
	if r.status != 201 {
		t.Fatalf("%d %v", r.status, r.body)
	}
	waitFor(t, func() bool { return e.tg.count() == 1 && e.dc.count() == 1 })
	if first := e.dc.first(); first.Priority != core.PriorityHigh || first.Kind != "note" {
		t.Fatalf("envelope %+v", first)
	}
	// Explicit list.
	e.do("POST", "/api/v1/messages", tokNotes, `{"kind":"note","data":{"text":"only dc"},"senders":["dc"]}`)
	waitFor(t, func() bool { return e.dc.count() == 2 })
	// Empty list: stored, not sent.
	r = e.do("POST", "/api/v1/messages", tokNotes, `{"kind":"note","data":{"text":"quiet"},"senders":[]}`)
	if r.status != 201 || len(r.body["deliveries"].(map[string]any)) != 0 {
		t.Fatalf("store-only: %v", r.body)
	}
	if r := e.do("POST", "/api/v1/messages", tokNotes, `{"kind":"note","data":{"text":"x"},"senders":["nope"]}`); errCode(r) != "unknown_sender" {
		t.Fatalf("unknown sender: %v", r.body)
	}
}

func TestErrors(t *testing.T) {
	e := setup(t)
	cases := []struct {
		name, method, path, token, body string
		status                          int
		code                            string
	}{
		{"no token", "POST", "/api/v1/messages", "", `{}`, 401, "unauthorized"},
		{"bad token", "GET", "/api/v1/messages", "nope-nope-nope-nope", "", 401, "unauthorized"},
		{"read token cannot write", "POST", "/api/v1/messages", tokReader, `{"kind":"sms","data":{}}`, 403, "forbidden"},
		{"write token cannot read", "GET", "/api/v1/messages", tokPhone, "", 403, "forbidden"},
		{"kind not allowed for token", "POST", "/api/v1/messages", tokPhone, `{"kind":"note","data":{"text":"x"}}`, 403, "forbidden"},
		{"bad json", "POST", "/api/v1/messages", tokPhone, `{`, 400, "invalid_json"},
		{"unknown top-level field", "POST", "/api/v1/messages", tokPhone, `{"kind":"sms","data":{},"x":1}`, 400, "invalid_json"},
		{"missing kind", "POST", "/api/v1/messages", tokAdmin, `{"data":{}}`, 400, "validation_failed"},
		{"missing data", "POST", "/api/v1/messages", tokAdmin, `{"kind":"sms"}`, 400, "validation_failed"},
		{"unknown kind", "POST", "/api/v1/messages", tokAdmin, `{"kind":"fax","data":{}}`, 400, "unknown_kind"},
		{"invalid data", "POST", "/api/v1/messages", tokPhone, `{"kind":"sms","data":{"from":"x"}}`, 400, "validation_failed"},
		{"too large", "POST", "/api/v1/messages", tokPhone, `{"kind":"sms","data":{"from":"x","body":"` + strings.Repeat("a", 70000) + `"}}`, 413, "payload_too_large"},
		{"bad limit", "GET", "/api/v1/messages?limit=abc", tokReader, "", 400, "validation_failed"},
		{"missing message", "GET", "/api/v1/messages/999", tokReader, "", 404, "not_found"},
		{"unknown endpoint", "GET", "/api/v1/nope", tokReader, "", 404, "not_found"},
		{"reader cannot delete", "DELETE", "/api/v1/messages/1", tokReader, "", 403, "forbidden"},
	}
	for _, c := range cases {
		r := e.do(c.method, c.path, c.token, c.body)
		if r.status != c.status || errCode(r) != c.code {
			t.Errorf("%s: got %d %q, want %d %q (%v)", c.name, r.status, errCode(r), c.status, c.code, r.body)
		}
	}
	r := e.do("POST", "/api/v1/messages", tokPhone, `{"kind":"sms","data":{"from":"x"}}`)
	if f := r.body["error"].(map[string]any)["field"]; f != "data.body" {
		t.Errorf("error field = %v", f)
	}
}

func TestKindVisibility(t *testing.T) {
	e := setup(t)
	e.do("POST", "/api/v1/messages", tokPhone, `{"kind":"sms","data":{"from":"a","body":"secret sms"}}`)
	e.do("POST", "/api/v1/messages", tokNotes, `{"kind":"note","data":{"text":"a note"}}`)

	// notes-only token sees only notes, even when asking for sms.
	for _, q := range []string{"", "?kind=sms", "?kind=sms,note"} {
		r := e.do("GET", "/api/v1/messages"+q, tokNotes, "")
		for _, it := range items(r) {
			if it["kind"] != "note" {
				t.Fatalf("%s leaked %v", q, it)
			}
		}
	}
	if r := e.do("GET", "/api/v1/messages/1", tokNotes, ""); r.status != 404 {
		t.Fatalf("hidden message: %d", r.status)
	}
	if r := e.do("GET", "/api/v1/kinds", tokNotes, ""); len(items(r)) != 1 {
		t.Fatalf("kinds: %v", r.body)
	}
	// Full reader sees both; filters work.
	if r := e.do("GET", "/api/v1/messages", tokReader, ""); len(items(r)) != 2 {
		t.Fatalf("reader: %v", r.body)
	}
	if r := e.do("GET", "/api/v1/messages?kind=note", tokReader, ""); len(items(r)) != 1 {
		t.Fatalf("kind filter: %v", r.body)
	}
	if r := e.do("GET", "/api/v1/messages?source=iphone-backup", tokReader, ""); len(items(r)) != 1 {
		t.Fatalf("source filter: %v", r.body)
	}
	if r := e.do("GET", "/api/v1/messages?q="+url.QueryEscape("secret"), tokReader, ""); len(items(r)) != 1 {
		t.Fatalf("search: %v", r.body)
	}
}

func TestPagination(t *testing.T) {
	e := setup(t)
	for i := 0; i < 5; i++ {
		e.do("POST", "/api/v1/messages", tokNotes, `{"kind":"note","data":{"text":"n`+string(rune('0'+i))+`"},"senders":[]}`)
	}
	r := e.do("GET", "/api/v1/messages?limit=2", tokReader, "")
	if len(items(r)) != 2 || r.body["next_before"].(float64) != 4 {
		t.Fatalf("page1: %v", r.body)
	}
	r = e.do("GET", "/api/v1/messages?limit=2&before=4", tokReader, "")
	if ids := items(r); len(ids) != 2 || ids[0]["id"].(float64) != 3 {
		t.Fatalf("page2: %v", r.body)
	}
	r = e.do("GET", "/api/v1/messages?since=3", tokReader, "")
	if len(items(r)) != 2 || r.body["next_before"] != nil {
		t.Fatalf("since: %v", r.body)
	}
}

func TestRetryRedeliverDelete(t *testing.T) {
	e := setup(t)
	e.tg.set(1, false) // first attempt fails, retry succeeds
	e.do("POST", "/api/v1/messages", tokPhone, `{"kind":"sms","data":{"from":"a","body":"retry me"}}`)
	waitFor(t, func() bool { return e.tg.count() == 1 })
	r := e.do("GET", "/api/v1/messages/1/deliveries", tokReader, "")
	if d := items(r)[0]; d["status"] != "sent" || d["attempts"].(float64) != 2 {
		t.Fatalf("deliveries: %v", r.body)
	}

	// Permanent failure, then manual redelivery to another sender.
	e.tg.set(0, true)
	e.do("POST", "/api/v1/messages", tokPhone, `{"kind":"sms","data":{"from":"a","body":"fails"}}`)
	waitFor(t, func() bool {
		return e.do("GET", "/api/v1/messages/2", tokReader, "").body["deliveries"].(map[string]any)["tg"] == "failed"
	})
	if r := e.do("POST", "/api/v1/messages/2/deliveries", tokReader, `{}`); r.status != 403 {
		t.Fatalf("redeliver needs scope: %d", r.status)
	}
	if r := e.do("POST", "/api/v1/messages/2/deliveries", tokAdmin, `{"senders":["dc"]}`); r.status != 202 {
		t.Fatalf("redeliver: %d %v", r.status, r.body)
	}
	waitFor(t, func() bool { return e.dc.count() == 1 })
	e.tg.set(0, false)
	if r := e.do("POST", "/api/v1/messages/2/deliveries", tokAdmin, ``); r.status != 202 || r.body["queued"].(float64) != 1 {
		t.Fatalf("retry failed: %v", r.body)
	}
	waitFor(t, func() bool { return e.tg.count() == 2 })

	if r := e.do("DELETE", "/api/v1/messages/2", tokAdmin, ""); r.status != 204 {
		t.Fatalf("delete: %d", r.status)
	}
	if r := e.do("GET", "/api/v1/messages/2", tokAdmin, ""); r.status != 404 {
		t.Fatalf("after delete: %d", r.status)
	}
}

func TestFormAndMeta(t *testing.T) {
	e := setup(t)
	req, _ := http.NewRequest("POST", e.url+"/api/v1/messages",
		strings.NewReader(url.Values{"kind": {"sms"}, "from": {"10086"}, "body": {"form body"}}.Encode()))
	req.Header.Set("Authorization", "Bearer "+tokPhone)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != 201 {
		t.Fatalf("form: %v %v", res.StatusCode, err)
	}
	res.Body.Close()

	if r := e.do("GET", "/api/v1/me", tokPhone, ""); r.body["name"] != "iphone-backup" || r.body["via"] != "token" {
		t.Fatalf("me: %v", r.body)
	}
	if r := e.do("GET", "/api/v1/senders", tokPhone, ""); len(items(r)) != 2 {
		t.Fatalf("senders: %v", r.body)
	}
	r := e.do("GET", "/api/v1/kinds", tokReader, "")
	ks := items(r)
	if len(ks) != 2 || ks[1]["name"] != "sms" || ks[1]["default_senders"].([]any)[0] != "tg" {
		t.Fatalf("kinds: %v", r.body)
	}
	if r := e.do("GET", "/api/v1/messages", tokReader, ""); r.header.Get("Cache-Control") != "no-store" {
		t.Fatal("missing no-store")
	}
	res, _ = http.Get(e.url + "/")
	if res.StatusCode != 200 {
		t.Fatalf("web: %d", res.StatusCode)
	}
	res.Body.Close()
}

func TestSenderManagement(t *testing.T) {
	e := setup(t)

	// Readers see names and state only; managers see config but never secrets.
	r := e.do("GET", "/api/v1/senders", tokReader, "")
	if it := items(r)[0]; it["config"] != nil || it["status"] != "active" {
		t.Fatalf("reader view: %v", it)
	}
	r = e.do("GET", "/api/v1/senders", tokAdmin, "")
	it := items(r)[0]
	if _, leaked := it["config"].(map[string]any)["secret"]; leaked || it["secrets_set"].([]any)[0] != "secret" {
		t.Fatalf("admin view: %v", it)
	}
	raw, _ := json.Marshal(r.body)
	if strings.Contains(string(raw), "s3cret") {
		t.Fatal("secret value returned by the API")
	}

	cases := []struct {
		name, method, path, token, body string
		status                          int
		code                            string
	}{
		{"reader cannot create", "POST", "/api/v1/senders", tokReader, `{"name":"x","type":"fake"}`, 403, "forbidden"},
		{"phone cannot create", "POST", "/api/v1/senders", tokPhone, `{"name":"x","type":"fake"}`, 403, "forbidden"},
		{"bad name", "POST", "/api/v1/senders", tokAdmin, `{"name":"Bad Name","type":"fake"}`, 400, "validation_failed"},
		{"unknown type", "POST", "/api/v1/senders", tokAdmin, `{"name":"x","type":"pigeon"}`, 400, "validation_failed"},
		{"unknown config field", "POST", "/api/v1/senders", tokAdmin, `{"name":"x","type":"fake","config":{"zzz":1}}`, 400, "validation_failed"},
		{"build rejects config", "POST", "/api/v1/senders", tokAdmin, `{"name":"x","type":"fake","config":{"note":"bad"}}`, 400, "validation_failed"},
		{"duplicate", "POST", "/api/v1/senders", tokAdmin, `{"name":"tg","type":"fake"}`, 409, "conflict"},
		{"update missing", "PATCH", "/api/v1/senders/nope", tokAdmin, `{"enabled":false}`, 404, "not_found"},
		{"change type", "PATCH", "/api/v1/senders/tg", tokAdmin, `{"type":"other"}`, 400, "validation_failed"},
		{"route unknown kind", "PUT", "/api/v1/routes/fax", tokAdmin, `{"senders":["tg"]}`, 404, "not_found"},
		{"route unknown sender", "PUT", "/api/v1/routes/sms", tokAdmin, `{"senders":["nope"]}`, 400, "validation_failed"},
		{"chats unsupported", "GET", "/api/v1/senders/tg/chats", tokAdmin, "", 400, "unsupported"},
		{"types need scope", "GET", "/api/v1/sender-types", tokReader, "", 403, "forbidden"},
	}
	for _, c := range cases {
		r := e.do(c.method, c.path, c.token, c.body)
		if r.status != c.status || errCode(r) != c.code {
			t.Errorf("%s: got %d %q, want %d %q (%v)", c.name, r.status, errCode(r), c.status, c.code, r.body)
		}
	}

	// Create a disabled sender, then enable it.
	r = e.do("POST", "/api/v1/senders", tokAdmin, `{"name":"extra","type":"fake","enabled":false,"config":{"note":"hi"}}`)
	if r.status != 201 || r.body["status"] != "disabled" {
		t.Fatalf("create: %d %v", r.status, r.body)
	}
	if r := e.do("POST", "/api/v1/senders/extra/test", tokAdmin, ""); r.status != 409 || errCode(r) != "sender_inactive" {
		t.Fatalf("test disabled: %d %v", r.status, r.body)
	}
	if r := e.do("POST", "/api/v1/messages", tokNotes, `{"kind":"note","data":{"text":"x"},"senders":["extra"]}`); errCode(r) != "unknown_sender" {
		t.Fatalf("disabled sender accepted: %v", r.body)
	}
	if r := e.do("PATCH", "/api/v1/senders/extra", tokAdmin, `{"enabled":true}`); r.body["status"] != "active" || r.body["config"].(map[string]any)["note"] != "hi" {
		t.Fatalf("enable: %v", r.body)
	}

	// Updating without the secret keeps it.
	r = e.do("PATCH", "/api/v1/senders/tg", tokAdmin, `{"config":{"note":"changed","secret":""}}`)
	if r.status != 200 || len(r.body["secrets_set"].([]any)) != 1 {
		t.Fatalf("secret kept: %v", r.body)
	}

	// Test sends through the sender.
	if r := e.do("POST", "/api/v1/senders/tg/test", tokAdmin, ""); r.body["ok"] != true || e.tg.count() != 1 {
		t.Fatalf("test: %v", r.body)
	}
	e.tg.set(0, true)
	if r := e.do("POST", "/api/v1/senders/tg/test", tokAdmin, ""); r.body["ok"] != false || r.body["error"] == "" {
		t.Fatalf("failing test: %v", r.body)
	}
	e.tg.set(0, false)

	// Routes: sms -> dc only, then back to default.
	r = e.do("PUT", "/api/v1/routes/sms", tokAdmin, `{"senders":["dc"]}`)
	if r.status != 200 || r.body["effective"].([]any)[0] != "dc" {
		t.Fatalf("set route: %v", r.body)
	}
	e.do("POST", "/api/v1/messages", tokPhone, `{"kind":"sms","data":{"from":"a","body":"routed"}}`)
	waitFor(t, func() bool { return e.dc.count() == 1 })
	if r := e.do("PUT", "/api/v1/routes/sms", tokAdmin, `{"senders":[]}`); r.body["custom"] != true || len(r.body["effective"].([]any)) != 0 {
		t.Fatalf("empty route: %v", r.body)
	}
	r = e.do("PUT", "/api/v1/routes/sms", tokAdmin, `{"senders":null}`)
	if r.body["custom"] != false || len(r.body["effective"].([]any)) != 3 {
		t.Fatalf("reset route: %v", r.body)
	}

	// Deleting a sender removes it from routes.
	e.do("PUT", "/api/v1/routes/note", tokAdmin, `{"senders":["dc","tg"]}`)
	if r := e.do("DELETE", "/api/v1/senders/dc", tokAdmin, ""); r.status != 204 {
		t.Fatalf("delete: %d", r.status)
	}
	r = e.do("GET", "/api/v1/routes", tokAdmin, "")
	for _, it := range items(r) {
		if it["kind"] == "note" && (len(it["senders"].([]any)) != 1 || it["senders"].([]any)[0] != "tg") {
			t.Fatalf("route after delete: %v", it)
		}
	}
}
