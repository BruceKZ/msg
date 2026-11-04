package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func checkConfig(t *testing.T, cfg string) error {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := build(p, slog.New(slog.NewTextHandler(io.Discard, nil)), true)
	if a != nil {
		a.store.Close()
	}
	return err
}

func TestExampleConfig(t *testing.T) {
	raw, err := os.ReadFile("deploy/config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	cfg := strings.Replace(string(raw), "REPLACE: msg gentoken", "aaaaaaaaaaaaaaaaaaaaaaaa", 1)
	cfg = strings.Replace(cfg, "REPLACE: msg gentoken", "bbbbbbbbbbbbbbbbbbbbbbbb", 1)
	if err := checkConfig(t, cfg); err != nil {
		t.Fatalf("example config: %v", err)
	}
}

func TestConfigErrors(t *testing.T) {
	base := `"db_path":"/x/msg.db","tokens":[{"name":"a","token":"aaaaaaaaaaaaaaaaaaaa","scopes":["messages:read"]}]`
	tg := `{"type":"telegram","name":"tg","bot_token":"x","chat_ids":[1]}`
	cases := map[string]string{
		"unknown field":          `{` + base + `,"oops":1}`,
		"unknown kind":           `{` + base + `,"kinds":{"fax":{}}}`,
		"unknown sender type":    `{` + base + `,"senders":[{"type":"pigeon","name":"p"}]}`,
		"duplicate sender":       `{` + base + `,"senders":[` + tg + `,` + tg + `]}`,
		"route to unknown":       `{` + base + `,"senders":[` + tg + `],"routes":{"sms":["dc"]}}`,
		"route for unknown kind": `{` + base + `,"senders":[` + tg + `],"routes":{"fax":["tg"]}}`,
		"half access":            `{` + base + `,"access":{"team_domain":"x.cloudflareaccess.com"}}`,
		"bad access scope":       `{` + base + `,"access":{"scopes":["root"]}}`,
		"telegram no token":      `{` + base + `,"senders":[{"type":"telegram","name":"tg"}]}`,
		"bad timezone":           `{` + base + `,"timezone":"Mars/Base"}`,
	}
	for name, cfg := range cases {
		if err := checkConfig(t, cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A disabled sender may stay in routes.
	ok := `{` + base + `,"senders":[{"type":"telegram","name":"tg","enabled":false}],"routes":{"sms":["tg"]}}`
	if err := checkConfig(t, ok); err != nil {
		t.Errorf("disabled sender in route: %v", err)
	}
}
