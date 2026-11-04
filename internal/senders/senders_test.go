package senders

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"msg/internal/core"
	"msg/internal/store"
)

var testType = Type{
	Name: "t",
	Fields: []Field{
		{Name: "token", Kind: FieldSecret, Required: true},
		{Name: "ids", Kind: FieldIntList},
		{Name: "flag", Kind: FieldBool},
		{Name: "label", Kind: FieldString},
	},
	Build: func(name string, cfg Config, env Env) (core.Sender, error) { return &nopSender{name}, nil },
}

type nopSender struct{ name string }

func (n *nopSender) Name() string                              { return n.name }
func (n *nopSender) Send(context.Context, core.Envelope) error { return nil }

func TestNormalize(t *testing.T) {
	c, err := normalize(testType, json.RawMessage(`{"token":" abc ","ids":"1, -100 2","flag":true}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.String("token") != "abc" || !reflect.DeepEqual(c.Ints("ids"), []int64{1, -100, 2}) || !c.Bool("flag") || c.String("label") != "" {
		t.Fatalf("got %#v", c)
	}
	for _, ids := range []string{`[1,"2",-3]`, `"1 2 -3"`} {
		c, err := normalize(testType, json.RawMessage(`{"token":"x","ids":`+ids+`}`), nil)
		if err != nil || len(c.Ints("ids")) != 3 {
			t.Errorf("ids %s: %v %v", ids, c, err)
		}
	}
	// Update: omitted fields and an empty secret keep their old values.
	upd, err := normalize(testType, json.RawMessage(`{"token":"","label":"new"}`), c)
	if err != nil || upd.String("token") != "abc" || upd.String("label") != "new" || !upd.Bool("flag") {
		t.Fatalf("update: %#v %v", upd, err)
	}
	for _, bad := range []string{`{}`, `{"token":"x","ids":"1,a"}`, `{"token":"x","flag":"yes"}`, `{"token":"x","nope":1}`, `[]`} {
		if _, err := normalize(testType, json.RawMessage(bad), nil); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	pub, set := public(testType, c)
	if _, ok := pub["token"]; ok || len(set) != 1 {
		t.Fatalf("public leaked secret: %v %v", pub, set)
	}
}

func TestPersistence(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	env := Env{Loc: time.UTC, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx := context.Background()
	m, err := New(ctx, st, []Type{testType}, env)
	if err != nil {
		t.Fatal(err)
	}
	off := false
	if _, err := m.Create(ctx, Change{Name: "a", Type: "t", Config: json.RawMessage(`{"token":"secret","ids":[5]}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(ctx, Change{Name: "b", Type: "t", Enabled: &off, Config: json.RawMessage(`{"token":"x"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := m.SetRoute(ctx, "sms", []string{"b", "a"}); err != nil {
		t.Fatal(err)
	}

	// A fresh manager (service restart) restores everything from the database.
	m2, err := New(ctx, st, []Type{testType}, env)
	if err != nil {
		t.Fatal(err)
	}
	if got := m2.ActiveNames(); !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("active after reload: %v", got)
	}
	if got := m2.DefaultSenders("sms"); !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("route skips disabled: %v", got)
	}
	if got := m2.DefaultSenders("other"); !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("no route = all active: %v", got)
	}
	info, _ := m2.Get("a", true)
	if !reflect.DeepEqual(info.SecretsSet, []string{"token"}) || info.Config["ids"] == nil {
		t.Fatalf("info: %+v", info)
	}
	row, _ := st.Sender(ctx, "a")
	if !json.Valid(row.Config) {
		t.Fatal("stored config not JSON")
	}
}
