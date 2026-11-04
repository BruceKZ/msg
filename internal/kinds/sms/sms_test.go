package sms

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"msg/internal/core"
)

func decode(t *testing.T, data string, now time.Time) (SMS, error) {
	t.Helper()
	m, err := Kind{}.Decode(json.RawMessage(data), core.Meta{Source: "phone", Now: now})
	if err != nil {
		return SMS{}, err
	}
	return m.(SMS), nil
}

func TestDecode(t *testing.T) {
	now := time.Date(2026, 11, 4, 13, 0, 0, 0, time.UTC)

	s, err := decode(t, `{"from":" 95588 ","body":" hi ","received_at":"2026-11-04T13:59:00+01:00"}`, now)
	if err != nil {
		t.Fatal(err)
	}
	if s.From != "95588" || s.Body != "hi" || !s.ReceivedAt.Equal(now.Add(-time.Minute)) {
		t.Fatalf("got %+v", s)
	}

	for _, ts := range []string{`1793797140`, `1793797140000`, `"1793797140"`, `"2026-11-04 13:59:00 +0100"`} {
		s, err := decode(t, `{"from":"a","body":"b","received_at":`+ts+`}`, now)
		if err != nil || !s.ReceivedAt.Equal(time.Unix(1793797140, 0)) {
			t.Errorf("received_at %s: got %v, %v", ts, s.ReceivedAt, err)
		}
	}

	// Missing, unparsable, or wildly wrong clocks fall back to server time.
	for _, ts := range []string{``, `,"received_at":null`, `,"received_at":"yesterday"`, `,"received_at":"2020-01-01T00:00:00Z"`} {
		s, err := decode(t, `{"from":"a","body":"b"`+ts+`}`, now)
		if err != nil || !s.ReceivedAt.Equal(now) {
			t.Errorf("fallback %q: got %v, %v", ts, s.ReceivedAt, err)
		}
	}
}

func TestDecodeValidation(t *testing.T) {
	cases := map[string]string{
		`{"body":"x"}`:             "data.from",
		`{"from":"  ","body":"x"}`: "data.from",
		`{"from":"a"}`:             "data.body",
		`{"from":1,"body":"x"}`:    "data.from",
		`[1,2]`:                    "data",
	}
	for in, field := range cases {
		_, err := decode(t, in, time.Now())
		var ve *core.ValidationError
		if !errors.As(err, &ve) || ve.Field != field {
			t.Errorf("%s: got %v, want validation error on %s", in, err, field)
		}
	}
}

func TestComposeAndDedup(t *testing.T) {
	now := time.Now().UTC()
	s := SMS{From: "Google", Body: "G-583920", ReceivedAt: now}
	e := s.Compose(core.Meta{Source: "iphone-backup"})
	if e.Title != "Google" || e.Body != "G-583920" || !e.Time.Equal(now) || len(e.Fields) != 1 || e.Fields[0].Value != "iphone-backup" {
		t.Fatalf("envelope %+v", e)
	}
	same := SMS{From: "Google", Body: "G-583920", ReceivedAt: now.Add(time.Second)}
	other := SMS{From: "Google", Body: "G-583921"}
	if s.DedupKey() != same.DedupKey() || s.DedupKey() == other.DedupKey() {
		t.Fatal("dedup key should depend on from+body only")
	}
}
