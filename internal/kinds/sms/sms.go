// Package sms is the message kind for SMS forwarded from a phone.
package sms

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"msg/internal/core"
)

const Name = "sms"

// SMS is the data of one text message.
type SMS struct {
	From       string    `json:"from"`
	Body       string    `json:"body"`
	ReceivedAt time.Time `json:"received_at"`
}

func (s SMS) Compose(meta core.Meta) core.Envelope {
	return core.Envelope{
		Title:  s.From,
		Body:   s.Body,
		Time:   s.ReceivedAt,
		Fields: []core.Field{{Name: "来源", Value: meta.Source}},
	}
}

// DedupKey makes repeated submissions of the same text (one SMS firing several
// Shortcut automations) collapse into one message.
func (s SMS) DedupKey() string {
	h := sha256.Sum256([]byte(s.From + "\x00" + s.Body))
	return hex.EncodeToString(h[:16])
}

type Kind struct{}

func New() Kind { return Kind{} }

func (Kind) Info() core.KindInfo {
	return core.KindInfo{
		Name:        Name,
		Description: "手机收到的短信",
		Fields: []core.FieldInfo{
			{Name: "from", Type: "string", Required: true, Description: "发件人号码或名字"},
			{Name: "body", Type: "string", Required: true, Description: "短信正文"},
			{Name: "received_at", Type: "time", Description: "收到时间；缺失或与服务器时间相差超过一天时用服务器时间"},
		},
	}
}

const maxClockSkew = 24 * time.Hour

func (Kind) Decode(data json.RawMessage, meta core.Meta) (core.Message, error) {
	var in struct {
		From       *string         `json:"from"`
		Body       *string         `json:"body"`
		ReceivedAt json.RawMessage `json:"received_at"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&in); err != nil {
		var te *json.UnmarshalTypeError
		if errors.As(err, &te) && te.Field != "" {
			return nil, &core.ValidationError{Field: "data." + te.Field, Message: "must be a " + te.Type.String()}
		}
		return nil, &core.ValidationError{Field: "data", Message: "must be a JSON object"}
	}
	s := SMS{}
	if in.From != nil {
		s.From = strings.TrimSpace(*in.From)
	}
	if in.Body != nil {
		s.Body = strings.TrimSpace(*in.Body)
	}
	switch {
	case s.From == "":
		return nil, &core.ValidationError{Field: "data.from", Message: "is required"}
	case s.Body == "":
		return nil, &core.ValidationError{Field: "data.body", Message: "is required"}
	case len(s.From) > 256:
		return nil, &core.ValidationError{Field: "data.from", Message: "is too long"}
	}

	s.ReceivedAt = meta.Now
	if t, ok := parseTime(in.ReceivedAt); ok {
		if d := meta.Now.Sub(t); d < maxClockSkew && d > -maxClockSkew {
			s.ReceivedAt = t.UTC()
		}
	}
	return s, nil
}

// parseTime accepts unix seconds or milliseconds (number or numeric string)
// and common date strings, including the format iOS Shortcuts produces.
func parseTime(raw json.RawMessage) (time.Time, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return time.Time{}, false
	}
	var str string
	if err := json.Unmarshal(raw, &str); err != nil {
		str = string(raw) // a bare number
	}
	str = strings.TrimSpace(str)
	if str == "" {
		return time.Time{}, false
	}
	if n, err := strconv.ParseFloat(str, 64); err == nil && n > 0 {
		if n < 1e12 {
			return time.UnixMilli(int64(n * 1000)), true
		}
		return time.UnixMilli(int64(n)), true
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05Z0700",
		"2006-01-02 15:04:05 -0700",
		"2006-01-02 15:04:05Z07:00",
	} {
		if t, err := time.Parse(layout, str); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
