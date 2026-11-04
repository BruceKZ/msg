// Package core defines the types shared by every part of the system.
//
// Messages enter through a Kind (input adapter), which validates the
// kind-specific data and composes it into a generic Envelope. Senders
// (output adapters) only ever see Envelopes. Nothing in the core knows
// about any concrete kind or sender.
package core

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

type Priority string

const (
	PriorityNormal Priority = "normal"
	PriorityHigh   Priority = "high"
)

type Field struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Envelope is the one generic notification format stored and delivered by the system.
type Envelope struct {
	Kind     string    `json:"kind"`
	Title    string    `json:"title"`
	Body     string    `json:"body"`
	Fields   []Field   `json:"fields,omitempty"`
	Time     time.Time `json:"time"`
	Priority Priority  `json:"priority"`
	Link     string    `json:"link,omitempty"`
}

// Meta carries generic information about a submission that kinds may use when composing.
type Meta struct {
	Source string    // name of the token that submitted the message
	Now    time.Time // server time of the submission
}

// Message is a decoded, validated message of some kind.
// It must be JSON-serialisable: its JSON form is stored as the message's data.
type Message interface {
	Compose(Meta) Envelope
}

// Deduper may be implemented by a Message to enable automatic de-duplication.
type Deduper interface {
	DedupKey() string
}

type FieldInfo struct {
	Name        string `json:"name"`
	Type        string `json:"type"` // string | time | number | object ...
	Required    bool   `json:"required"`
	Description string `json:"description,omitempty"`
}

type KindInfo struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Fields      []FieldInfo `json:"fields"`
}

// Kind is the input adapter for one message type.
type Kind interface {
	Info() KindInfo
	// Decode validates data and builds a Message. Validation problems are
	// returned as *ValidationError.
	Decode(data json.RawMessage, meta Meta) (Message, error)
}

// Sender is the output adapter for one delivery channel.
type Sender interface {
	Name() string
	Send(ctx context.Context, e Envelope) error
}

// ValidationError reports a problem with one field of the submitted data.
type ValidationError struct {
	Field   string // e.g. "data.body"
	Message string
}

func (e *ValidationError) Error() string { return e.Field + ": " + e.Message }

// Permanent wraps a send error that retrying will not fix.
type Permanent struct{ Err error }

func (p Permanent) Error() string { return p.Err.Error() }
func (p Permanent) Unwrap() error { return p.Err }

var (
	ErrUnknownKind   = errors.New("unknown kind")
	ErrUnknownSender = errors.New("unknown sender")
)
