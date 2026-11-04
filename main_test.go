package main

import (
	"context"
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
	a, err := build(context.Background(), p, slog.New(slog.NewTextHandler(io.Discard, nil)), true)
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
	cases := map[string]string{
		"unknown field":      `{` + base + `,"oops":1}`,
		"legacy senders key": `{` + base + `,"senders":[]}`,
		"legacy routes key":  `{` + base + `,"routes":{}}`,
		"unknown kind":       `{` + base + `,"kinds":{"fax":{}}}`,
		"half access":        `{` + base + `,"access":{"team_domain":"x.cloudflareaccess.com"}}`,
		"bad access scope":   `{` + base + `,"access":{"scopes":["root"]}}`,
		"bad token scope":    `{"db_path":"/x/msg.db","tokens":[{"name":"a","token":"aaaaaaaaaaaaaaaaaaaa","scopes":["admin"]}]}`,
		"bad timezone":       `{` + base + `,"timezone":"Mars/Base"}`,
		"no db path":         `{"tokens":[]}`,
	}
	for name, cfg := range cases {
		if err := checkConfig(t, cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := checkConfig(t, `{`+base+`}`); err != nil {
		t.Errorf("minimal config: %v", err)
	}
}
