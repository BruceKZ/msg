package senders

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"

	"msg/internal/core"
	"msg/internal/store"
)

// Manager owns the active sender instances and the per-kind routes.
type Manager struct {
	store *store.Store
	types map[string]Type
	order []string
	env   Env
	base  context.Context // parent context for background runners

	op        sync.Mutex   // serialises configuration changes
	mu        sync.RWMutex // guards the fields below
	rows      map[string]store.SenderRow
	active    map[string]*instance
	buildErrs map[string]string
	routes    map[string][]string
}

type instance struct {
	sender core.Sender
	cancel context.CancelFunc
	done   chan struct{}
}

// New loads every stored sender and starts the enabled ones. Background
// runners stop when ctx is cancelled.
func New(ctx context.Context, st *store.Store, types []Type, env Env) (*Manager, error) {
	m := &Manager{
		store: st, types: map[string]Type{}, env: env, base: ctx,
		rows: map[string]store.SenderRow{}, active: map[string]*instance{}, buildErrs: map[string]string{},
	}
	for _, t := range types {
		if _, dup := m.types[t.Name]; dup {
			return nil, fmt.Errorf("sender type %q registered twice", t.Name)
		}
		m.types[t.Name] = t
		m.order = append(m.order, t.Name)
	}
	sort.Strings(m.order)

	rows, err := st.Senders(ctx)
	if err != nil {
		return nil, err
	}
	routes, err := st.Routes(ctx)
	if err != nil {
		return nil, err
	}
	m.routes = routes
	for _, r := range rows {
		m.rows[r.Name] = r
		if r.Enabled {
			if err := m.start(r); err != nil {
				m.buildErrs[r.Name] = err.Error()
				env.Log.Error("sender not started", "sender", r.Name, "err", err)
			}
		}
	}
	return m, nil
}

// ---- used by the hub and dispatcher ----

// Lookup returns an active (enabled and working) sender.
func (m *Manager) Lookup(name string) (core.Sender, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	in, ok := m.active[name]
	if !ok {
		return nil, false
	}
	return in.sender, true
}

func (m *Manager) HasSender(name string) bool { _, ok := m.Lookup(name); return ok }

// ActiveNames lists active senders, sorted.
func (m *Manager) ActiveNames() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.active))
	for n := range m.active {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// DefaultSenders is where messages of kind go when the request names no senders:
// the kind's route (active members only) or, without a route, every active sender.
func (m *Manager) DefaultSenders(kind string) []string {
	m.mu.RLock()
	route, ok := m.routes[kind]
	m.mu.RUnlock()
	if !ok {
		return m.ActiveNames()
	}
	out := []string{}
	for _, n := range route {
		if m.HasSender(n) {
			out = append(out, n)
		}
	}
	return out
}

// ---- inspection ----

type Info struct {
	Name         string         `json:"name"`
	Type         string         `json:"type"`
	Enabled      bool           `json:"enabled"`
	Status       string         `json:"status"` // active | disabled | error
	Error        string         `json:"error,omitempty"`
	Config       map[string]any `json:"config,omitempty"`      // without secrets
	SecretsSet   []string       `json:"secrets_set,omitempty"` // which secret fields have a value
	CanListChats bool           `json:"can_list_chats,omitempty"`
	UpdatedAt    time.Time      `json:"updated_at"`
}

// List describes every configured sender. With details, non-secret config is included.
func (m *Manager) List(details bool) []Info {
	m.mu.RLock()
	defer m.mu.RUnlock()
	names := make([]string, 0, len(m.rows))
	for n := range m.rows {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]Info, 0, len(names))
	for _, n := range names {
		out = append(out, m.infoLocked(n, details))
	}
	return out
}

func (m *Manager) Get(name string, details bool) (Info, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.rows[name]; !ok {
		return Info{}, store.ErrNotFound
	}
	return m.infoLocked(name, details), nil
}

func (m *Manager) infoLocked(name string, details bool) Info {
	r := m.rows[name]
	in := Info{Name: r.Name, Type: r.Type, Enabled: r.Enabled, UpdatedAt: r.UpdatedAt}
	switch inst, ok := m.active[name]; {
	case ok:
		in.Status = "active"
		_, in.CanListChats = inst.sender.(ChatLister)
	case !r.Enabled:
		in.Status = "disabled"
	default:
		in.Status, in.Error = "error", m.buildErrs[name]
	}
	if details {
		if t, ok := m.types[r.Type]; ok {
			if cfg, err := decodeStored(t, r.Config); err == nil {
				in.Config, in.SecretsSet = public(t, cfg)
			}
		}
	}
	return in
}

func (m *Manager) Types() []Type {
	out := make([]Type, 0, len(m.order))
	for _, n := range m.order {
		out = append(out, m.types[n])
	}
	return out
}

func (m *Manager) Routes() map[string][]string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := map[string][]string{}
	for k, v := range m.routes {
		out[k] = slices.Clone(v)
	}
	return out
}

// ---- changes ----

type Change struct {
	Name    string          `json:"name"`
	Type    string          `json:"type"`
	Enabled *bool           `json:"enabled"`
	Config  json.RawMessage `json:"config"`
}

// Create validates, stores and (if enabled) starts a new sender.
func (m *Manager) Create(ctx context.Context, c Change) (Info, error) {
	m.op.Lock()
	defer m.op.Unlock()
	if err := validName(c.Name); err != nil {
		return Info{}, err
	}
	t, ok := m.types[c.Type]
	if !ok {
		return Info{}, &core.ValidationError{Field: "type", Message: "unknown sender type " + strconvQuote(c.Type)}
	}
	cfg, err := normalize(t, c.Config, nil)
	if err != nil {
		return Info{}, err
	}
	enabled := c.Enabled == nil || *c.Enabled
	sender, err := m.build(t, c.Name, cfg)
	if err != nil {
		return Info{}, err
	}
	raw, _ := json.Marshal(cfg)
	row := store.SenderRow{Name: c.Name, Type: c.Type, Enabled: enabled, Config: raw}
	if err := m.store.CreateSender(ctx, row); err != nil {
		return Info{}, err
	}
	row.UpdatedAt = time.Now().UTC()
	m.swap(row, sender)
	return m.Get(c.Name, true)
}

// Update changes a sender's enabled flag and/or configuration. Omitted fields,
// and empty secret fields, keep their stored values.
func (m *Manager) Update(ctx context.Context, name string, c Change) (Info, error) {
	m.op.Lock()
	defer m.op.Unlock()
	m.mu.RLock()
	row, ok := m.rows[name]
	m.mu.RUnlock()
	if !ok {
		return Info{}, store.ErrNotFound
	}
	if c.Type != "" && c.Type != row.Type {
		return Info{}, &core.ValidationError{Field: "type", Message: "cannot be changed; delete and recreate the sender"}
	}
	t, ok := m.types[row.Type]
	if !ok {
		return Info{}, fmt.Errorf("sender %s has unknown type %q", name, row.Type)
	}
	old, err := decodeStored(t, row.Config)
	if err != nil {
		return Info{}, err
	}
	cfg, err := normalize(t, c.Config, old)
	if err != nil {
		return Info{}, err
	}
	if c.Enabled != nil {
		row.Enabled = *c.Enabled
	}
	sender, err := m.build(t, name, cfg)
	if err != nil {
		return Info{}, err
	}
	row.Config, _ = json.Marshal(cfg)
	if err := m.store.UpdateSender(ctx, row); err != nil {
		return Info{}, err
	}
	row.UpdatedAt = time.Now().UTC()
	m.swap(row, sender)
	return m.Get(name, true)
}

func (m *Manager) Delete(ctx context.Context, name string) error {
	m.op.Lock()
	defer m.op.Unlock()
	if err := m.store.DeleteSender(ctx, name); err != nil {
		return err
	}
	m.mu.Lock()
	old := m.active[name]
	delete(m.active, name)
	delete(m.rows, name)
	delete(m.buildErrs, name)
	for kind, list := range m.routes {
		m.routes[kind] = slices.DeleteFunc(list, func(n string) bool { return n == name })
	}
	m.mu.Unlock()
	stop(old)
	return nil
}

// SetRoute sets the default sender list for a kind; nil resets it to "every sender".
func (m *Manager) SetRoute(ctx context.Context, kind string, names []string) error {
	m.op.Lock()
	defer m.op.Unlock()
	if names != nil {
		names = append([]string{}, slices.Compact(slices.Sorted(slices.Values(names)))...) // keep [] distinct from nil
		m.mu.RLock()
		for _, n := range names {
			if _, ok := m.rows[n]; !ok {
				m.mu.RUnlock()
				return &core.ValidationError{Field: "senders", Message: "unknown sender " + strconvQuote(n)}
			}
		}
		m.mu.RUnlock()
	}
	if err := m.store.SetRoute(ctx, kind, names); err != nil {
		return err
	}
	m.mu.Lock()
	if names == nil {
		delete(m.routes, kind)
	} else {
		m.routes[kind] = names
	}
	m.mu.Unlock()
	return nil
}

var ErrNotActive = errors.New("sender is not active")

// Test sends a test message through an active sender and reports the result.
func (m *Manager) Test(ctx context.Context, name, by string) error {
	s, ok := m.Lookup(name)
	if !ok {
		if _, err := m.Get(name, false); err != nil {
			return err
		}
		return ErrNotActive
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	return s.Send(ctx, core.Envelope{
		Kind:     "test",
		Title:    "msg 测试消息",
		Body:     "如果你看到这条消息，说明渠道 " + name + " 配置正确。",
		Fields:   []core.Field{{Name: "发起人", Value: by}},
		Time:     time.Now().UTC(),
		Priority: core.PriorityNormal,
	})
}

var ErrNoChats = errors.New("this sender type cannot list chats")

func (m *Manager) Chats(name string) ([]Chat, error) {
	s, ok := m.Lookup(name)
	if !ok {
		if _, err := m.Get(name, false); err != nil {
			return nil, err
		}
		return nil, ErrNotActive
	}
	cl, ok := s.(ChatLister)
	if !ok {
		return nil, ErrNoChats
	}
	return cl.RecentChats(), nil
}

// ---- internals ----

func (m *Manager) build(t Type, name string, cfg Config) (core.Sender, error) {
	s, err := t.Build(name, cfg, m.env)
	if err != nil {
		var ve *core.ValidationError
		if errors.As(err, &ve) {
			return nil, err
		}
		return nil, &core.ValidationError{Field: "config", Message: err.Error()}
	}
	return s, nil
}

// swap installs row and, if enabled, the freshly built sender, stopping any previous instance.
func (m *Manager) swap(row store.SenderRow, s core.Sender) {
	m.mu.Lock()
	old := m.active[row.Name]
	delete(m.active, row.Name)
	delete(m.buildErrs, row.Name)
	m.rows[row.Name] = row
	m.mu.Unlock()
	stop(old)

	if !row.Enabled {
		return
	}
	if old != nil {
		if cl, ok := old.sender.(ChatLister); ok {
			if seed, ok := s.(ChatSeeder); ok {
				seed.SeedChats(cl.RecentChats())
			}
		}
	}
	m.activate(row.Name, s)
}

// start builds and activates a stored sender (used at load time).
func (m *Manager) start(row store.SenderRow) error {
	t, ok := m.types[row.Type]
	if !ok {
		return fmt.Errorf("unknown sender type %q", row.Type)
	}
	cfg, err := decodeStored(t, row.Config)
	if err != nil {
		return err
	}
	s, err := m.build(t, row.Name, cfg)
	if err != nil {
		return err
	}
	m.activate(row.Name, s)
	return nil
}

func (m *Manager) activate(name string, s core.Sender) {
	ctx, cancel := context.WithCancel(m.base)
	in := &instance{sender: s, cancel: cancel, done: make(chan struct{})}
	if r, ok := s.(Runner); ok {
		go func() {
			defer close(in.done)
			r.Run(ctx)
		}()
	} else {
		close(in.done)
	}
	m.mu.Lock()
	m.active[name] = in
	m.mu.Unlock()
	m.env.Log.Info("sender active", "sender", name)
}

// stop cancels an instance's background work and waits briefly for it to end,
// so a rebuilt Telegram bot does not race the old one for updates.
func stop(in *instance) {
	if in == nil {
		return
	}
	in.cancel()
	select {
	case <-in.done:
	case <-time.After(3 * time.Second):
	}
}

func strconvQuote(s string) string { return fmt.Sprintf("%q", s) }
