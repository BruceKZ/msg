// Package senders manages sender instances at runtime: their configuration
// lives in the database, is edited through the API, and changes take effect
// immediately without restarting the service.
//
// Each sender type (telegram, discord, ...) describes its configuration
// fields, so clients can render a form without knowing the type.
package senders

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"msg/internal/core"
	"msg/internal/store"
)

type FieldKind string

const (
	FieldString  FieldKind = "string"
	FieldSecret  FieldKind = "secret"   // write-only: never returned by the API
	FieldIntList FieldKind = "int_list" // list of integers, e.g. chat ids
	FieldBool    FieldKind = "bool"
)

type Field struct {
	Name     string    `json:"name"`
	Label    string    `json:"label"`
	Kind     FieldKind `json:"kind"`
	Required bool      `json:"required"`
	Help     string    `json:"help,omitempty"`
}

// Inbox lets senders with chat commands read messages.
type Inbox interface {
	List(ctx context.Context, f store.Filter) ([]store.Message, error)
}

// Env is what a sender may use besides its own configuration.
type Env struct {
	Loc       *time.Location
	Log       *slog.Logger
	Inbox     Inbox
	KindNames func() []string
}

// Type describes one kind of sender and how to build it from its configuration.
type Type struct {
	Name        string  `json:"name"`
	Label       string  `json:"label"`
	Description string  `json:"description"`
	Fields      []Field `json:"fields"`
	// Build creates a sender from normalised configuration. Configuration
	// problems should be returned as *core.ValidationError.
	Build func(name string, cfg Config, env Env) (core.Sender, error) `json:"-"`
}

// Config is a normalised sender configuration: only declared fields, with Go types
// string, bool or []int64.
type Config map[string]any

func (c Config) String(name string) string { s, _ := c[name].(string); return s }
func (c Config) Bool(name string) bool     { b, _ := c[name].(bool); return b }
func (c Config) Ints(name string) []int64  { v, _ := c[name].([]int64); return v }

// Optional capabilities of a built sender.
type (
	// Runner is started in the background while the sender is active.
	Runner interface{ Run(ctx context.Context) }
	// ChatLister reports chats that recently talked to a bot, to help pick chat ids.
	ChatLister interface{ RecentChats() []Chat }
	// ChatSeeder receives the previous instance's chats when a sender is rebuilt.
	ChatSeeder interface{ SeedChats([]Chat) }
)

type Chat struct {
	ID       int64     `json:"id"`
	Title    string    `json:"title"`
	Type     string    `json:"type"`
	LastSeen time.Time `json:"last_seen"`
}

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

func validName(name string) error {
	if !nameRe.MatchString(name) {
		return &core.ValidationError{Field: "name", Message: "must be 1–32 characters: lowercase letters, digits, - or _"}
	}
	return nil
}

// normalize parses raw JSON config against the type's fields. old supplies
// values for secret fields that are omitted or empty in raw (keep the stored
// secret). Unknown fields are rejected.
func normalize(t Type, raw json.RawMessage, old Config) (Config, error) {
	in := map[string]json.RawMessage{}
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &in); err != nil {
			return nil, &core.ValidationError{Field: "config", Message: "must be a JSON object"}
		}
	}
	known := map[string]Field{}
	for _, f := range t.Fields {
		known[f.Name] = f
	}
	for k := range in {
		if _, ok := known[k]; !ok {
			return nil, &core.ValidationError{Field: "config." + k, Message: "unknown field"}
		}
	}

	out := Config{}
	for _, f := range t.Fields {
		v, present := in[f.Name]
		if present && string(v) == "null" {
			present = false
		}
		field := "config." + f.Name
		switch f.Kind {
		case FieldString, FieldSecret:
			var s string
			if present {
				if err := json.Unmarshal(v, &s); err != nil {
					return nil, &core.ValidationError{Field: field, Message: "must be a string"}
				}
			} else if old != nil {
				s = old.String(f.Name)
			}
			s = strings.TrimSpace(s)
			if s == "" && f.Kind == FieldSecret && old != nil {
				s = old.String(f.Name) // empty secret on update = keep
			}
			if s == "" && f.Required {
				return nil, &core.ValidationError{Field: field, Message: "is required"}
			}
			out[f.Name] = s
		case FieldBool:
			b := false
			if present {
				if err := json.Unmarshal(v, &b); err != nil {
					return nil, &core.ValidationError{Field: field, Message: "must be true or false"}
				}
			} else if old != nil {
				b = old.Bool(f.Name)
			}
			out[f.Name] = b
		case FieldIntList:
			list := []int64{}
			if present {
				parsed, err := parseIntList(v)
				if err != nil {
					return nil, &core.ValidationError{Field: field, Message: err.Error()}
				}
				list = parsed
			} else if old != nil {
				list = append(list, old.Ints(f.Name)...)
			}
			if len(list) == 0 && f.Required {
				return nil, &core.ValidationError{Field: field, Message: "is required"}
			}
			out[f.Name] = list
		default:
			return nil, fmt.Errorf("field %s: unsupported kind %q", f.Name, f.Kind)
		}
	}
	return out, nil
}

// parseIntList accepts [1, -100], ["1", "-100"] or "1, -100".
func parseIntList(v json.RawMessage) ([]int64, error) {
	var parts []string
	var s string
	if json.Unmarshal(v, &s) == nil {
		for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }) {
			parts = append(parts, p)
		}
	} else {
		var arr []json.RawMessage
		if err := json.Unmarshal(v, &arr); err != nil {
			return nil, fmt.Errorf("must be a list of integers")
		}
		for _, a := range arr {
			var str string
			if json.Unmarshal(a, &str) == nil {
				parts = append(parts, str)
			} else {
				parts = append(parts, string(a))
			}
		}
	}
	out := []int64{}
	for _, p := range parts {
		n, err := strconv.ParseInt(strings.TrimSpace(p), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not an integer", p)
		}
		out = append(out, n)
	}
	return out, nil
}

// decodeStored reads a stored config (already normalised) back into Go types.
func decodeStored(t Type, raw json.RawMessage) (Config, error) {
	return normalize(t, raw, nil)
}

// public returns the config with secret values removed, plus which secrets are set.
func public(t Type, c Config) (map[string]any, []string) {
	out := map[string]any{}
	set := []string{}
	for _, f := range t.Fields {
		if f.Kind == FieldSecret {
			if c.String(f.Name) != "" {
				set = append(set, f.Name)
			}
			continue
		}
		out[f.Name] = c[f.Name]
	}
	return out, set
}
