// Package hub turns submissions into stored messages with queued deliveries:
// it looks up the kind, decodes and composes the message, resolves which
// senders get it, and applies de-duplication.
package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"msg/internal/core"
	"msg/internal/store"
)

const DefaultDedupWindow = 2 * time.Minute

// Senders tells the hub which senders exist right now and where each kind goes
// by default. It is implemented by the runtime sender manager.
type Senders interface {
	HasSender(name string) bool
	DefaultSenders(kind string) []string
}

type Options struct {
	// DedupWindows overrides DefaultDedupWindow per kind; 0 disables de-duplication for that kind.
	DedupWindows map[string]time.Duration
	// Wake is called after a new message was queued for delivery.
	Wake func()
}

type Hub struct {
	store   *store.Store
	kinds   map[string]core.Kind
	order   []string // kind names, sorted
	senders Senders
	opts    Options
}

func New(st *store.Store, kinds []core.Kind, senders Senders, opts Options) (*Hub, error) {
	h := &Hub{store: st, kinds: map[string]core.Kind{}, senders: senders, opts: opts}
	for _, k := range kinds {
		name := k.Info().Name
		if _, dup := h.kinds[name]; dup {
			return nil, fmt.Errorf("kind %q registered twice", name)
		}
		h.kinds[name] = k
		h.order = append(h.order, name)
	}
	slices.Sort(h.order)
	if h.opts.Wake == nil {
		h.opts.Wake = func() {}
	}
	return h, nil
}

func (h *Hub) checkSenders(list []string) error {
	for _, s := range list {
		if !h.senders.HasSender(s) {
			return fmt.Errorf("%w %q", core.ErrUnknownSender, s)
		}
	}
	return nil
}

// Kinds describes every registered kind, with its default senders.
func (h *Hub) Kinds() []KindInfo {
	out := make([]KindInfo, 0, len(h.order))
	for _, name := range h.order {
		out = append(out, KindInfo{KindInfo: h.kinds[name].Info(), DefaultSenders: h.defaultSenders(name)})
	}
	return out
}

type KindInfo struct {
	core.KindInfo
	DefaultSenders []string `json:"default_senders"`
}

func (h *Hub) KindNames() []string { return slices.Clone(h.order) }

// Wake nudges the dispatcher, e.g. after deliveries were re-queued.
func (h *Hub) Wake() { h.opts.Wake() }

func (h *Hub) HasKind(name string) bool { _, ok := h.kinds[name]; return ok }

func (h *Hub) HasSender(name string) bool { return h.senders.HasSender(name) }

func (h *Hub) defaultSenders(kind string) []string { return h.senders.DefaultSenders(kind) }

type Submission struct {
	Kind     string
	Data     json.RawMessage
	Source   string
	Senders  []string // nil = default routing; empty = store only
	DedupKey string   // overrides the kind's own key
}

// Submit validates, stores and queues a message. Errors wrap core.ErrUnknownKind,
// core.ErrUnknownSender, or are *core.ValidationError.
func (h *Hub) Submit(ctx context.Context, sub Submission) (store.Message, bool, error) {
	kind, ok := h.kinds[sub.Kind]
	if !ok {
		return store.Message{}, false, fmt.Errorf("%w %q", core.ErrUnknownKind, sub.Kind)
	}
	senders := sub.Senders
	if senders == nil {
		senders = h.defaultSenders(sub.Kind)
	} else {
		senders = slices.Compact(slices.Sorted(slices.Values(senders)))
		if err := h.checkSenders(senders); err != nil {
			return store.Message{}, false, err
		}
	}

	meta := core.Meta{Source: sub.Source, Now: time.Now().UTC()}
	msg, err := kind.Decode(sub.Data, meta)
	if err != nil {
		return store.Message{}, false, err
	}
	env := msg.Compose(meta)
	env.Kind = sub.Kind
	if env.Time.IsZero() {
		env.Time = meta.Now
	}
	env.Time = env.Time.UTC()
	if env.Priority == "" {
		env.Priority = core.PriorityNormal
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return store.Message{}, false, fmt.Errorf("encode %s data: %w", sub.Kind, err)
	}

	key := strings.TrimSpace(sub.DedupKey)
	if key == "" {
		if d, ok := msg.(core.Deduper); ok {
			key = d.DedupKey()
		}
	}
	window := DefaultDedupWindow
	if w, ok := h.opts.DedupWindows[sub.Kind]; ok {
		window = w
	}

	m, dup, err := h.store.Insert(ctx, store.NewMessage{
		Kind: sub.Kind, Source: sub.Source, Data: data, Envelope: env,
		DedupKey: key, DedupWindow: window,
	}, senders)
	if err == nil && !dup && len(senders) > 0 {
		h.opts.Wake()
	}
	return m, dup, err
}
