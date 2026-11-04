// msg receives messages over HTTP, stores them, and forwards them to
// notification senders. See docs/design.md.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"syscall"
	"time"
	_ "time/tzdata" // OpenWrt ships no zoneinfo database

	"msg/internal/api"
	"msg/internal/auth"
	"msg/internal/core"
	"msg/internal/dispatch"
	"msg/internal/hub"
	"msg/internal/kinds/sms"
	"msg/internal/senders/telegram"
	"msg/internal/store"
	"msg/web"
)

var version = "dev"

// availableKinds lists every kind compiled into the binary.
func availableKinds() []core.Kind {
	return []core.Kind{sms.New()}
}

type Config struct {
	Listen   string        `json:"listen"`
	DBPath   string        `json:"db_path"`
	Timezone string        `json:"timezone"`
	Tokens   []auth.Token  `json:"tokens"`
	Access   *AccessConfig `json:"access"`
	Kinds    map[string]struct {
		Enabled            *bool `json:"enabled"`
		DedupWindowSeconds *int  `json:"dedup_window_seconds"`
	} `json:"kinds"`
	Senders []json.RawMessage   `json:"senders"`
	Routes  map[string][]string `json:"routes"`
}

type AccessConfig struct {
	auth.AccessConfig
	Scopes []string `json:"scopes"`
}

func main() {
	cfgPath := flag.String("config", "/etc/msg/config.json", "path to config file")
	flag.Parse()

	switch flag.Arg(0) {
	case "gentoken":
		b := make([]byte, 24)
		rand.Read(b)
		fmt.Println(base64.RawURLEncoding.EncodeToString(b))
		return
	case "version":
		fmt.Println(version)
		return
	case "check":
		log := slog.New(slog.NewTextHandler(os.Stderr, nil))
		if _, err := build(*cfgPath, log, true); err != nil {
			fmt.Fprintln(os.Stderr, "config error:", err)
			os.Exit(1)
		}
		fmt.Println("config ok")
		return
	case "":
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q (commands: gentoken, version, check)\n", flag.Arg(0))
		os.Exit(2)
	}

	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	if err := run(*cfgPath, log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func loadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := &Config{Listen: "127.0.0.1:8787", Timezone: "Local"}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if c.DBPath == "" {
		return nil, errors.New("db_path is required")
	}
	return c, nil
}

type app struct {
	store      *store.Store
	hub        *hub.Hub
	dispatcher *dispatch.Dispatcher
	server     *http.Server
	background []func(context.Context)
}

// build assembles everything from the config. With dryRun the database is
// opened in memory so the real one is not touched.
func build(cfgPath string, log *slog.Logger, dryRun bool) (*app, error) {
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return nil, err
	}
	loc, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		return nil, fmt.Errorf("timezone: %w", err)
	}

	dbPath := cfg.DBPath
	if dryRun {
		dbPath = ":memory:"
	} else if _, err := os.Stat(filepath.Dir(dbPath)); err != nil {
		// Never create the DB elsewhere (e.g. flash) when the data disk is not mounted.
		return nil, fmt.Errorf("db directory: %w", err)
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	// Kinds
	var kinds []core.Kind
	windows := map[string]time.Duration{}
	known := map[string]bool{}
	for _, k := range availableKinds() {
		name := k.Info().Name
		known[name] = true
		kc, ok := cfg.Kinds[name]
		if ok && kc.Enabled != nil && !*kc.Enabled {
			continue
		}
		kinds = append(kinds, k)
		if ok && kc.DedupWindowSeconds != nil {
			windows[name] = time.Duration(*kc.DedupWindowSeconds) * time.Second
		}
	}
	for name := range cfg.Kinds {
		if !known[name] {
			return nil, fmt.Errorf("kinds: unknown kind %q", name)
		}
	}

	// Senders
	var senders []core.Sender
	var infos []api.SenderInfo
	var bots []*telegram.Bot
	enabled := map[string]bool{}
	for i, raw := range cfg.Senders {
		var head struct {
			Type    string `json:"type"`
			Name    string `json:"name"`
			Enabled *bool  `json:"enabled"`
		}
		if err := json.Unmarshal(raw, &head); err != nil {
			return nil, fmt.Errorf("senders[%d]: %w", i, err)
		}
		if head.Name == "" {
			return nil, fmt.Errorf("senders[%d]: name is required", i)
		}
		if slices.ContainsFunc(infos, func(s api.SenderInfo) bool { return s.Name == head.Name }) {
			return nil, fmt.Errorf("senders[%d]: duplicate name %q", i, head.Name)
		}
		on := head.Enabled == nil || *head.Enabled
		infos = append(infos, api.SenderInfo{Name: head.Name, Type: head.Type, Enabled: on})
		switch head.Type {
		case "telegram":
		default:
			return nil, fmt.Errorf("senders[%d]: unknown type %q", i, head.Type)
		}
		if !on {
			continue
		}
		var tc struct {
			telegram.Config
			Type    string `json:"type"`
			Enabled *bool  `json:"enabled"`
		}
		if err := json.Unmarshal(raw, &tc); err != nil {
			return nil, fmt.Errorf("senders[%d]: %w", i, err)
		}
		bot, err := telegram.New(tc.Config, loc, log.With("sender", head.Name))
		if err != nil {
			return nil, err
		}
		senders = append(senders, bot)
		bots = append(bots, bot)
		enabled[head.Name] = true
	}

	// Routes may mention disabled senders (so a sender can be switched off
	// without editing routes); those are dropped here. Unknown names are errors.
	routes := map[string][]string{}
	for kind, list := range cfg.Routes {
		var keep []string
		for _, s := range list {
			if !slices.ContainsFunc(infos, func(i api.SenderInfo) bool { return i.Name == s }) {
				return nil, fmt.Errorf("routes.%s: unknown sender %q", kind, s)
			}
			if enabled[s] {
				keep = append(keep, s)
			} else {
				log.Warn("route skips disabled sender", "kind", kind, "sender", s)
			}
		}
		if keep == nil {
			keep = []string{}
		}
		routes[kind] = keep
	}

	disp := dispatch.New(st, senders, log)
	names := make([]string, 0, len(senders))
	for _, s := range senders {
		names = append(names, s.Name())
	}
	h, err := hub.New(st, kinds, names, hub.Options{Routes: routes, DedupWindows: windows, Wake: disp.Wake})
	if err != nil {
		return nil, err
	}

	var verifier *auth.AccessVerifier
	accessScopes := []string{auth.ScopeMessagesRead, auth.ScopeMessagesDelete, auth.ScopeDeliveriesWrite}
	if cfg.Access != nil {
		if cfg.Access.TeamDomain != "" || cfg.Access.AUD != "" {
			if cfg.Access.TeamDomain == "" || cfg.Access.AUD == "" {
				return nil, errors.New("access: team_domain and aud must both be set")
			}
			verifier = auth.NewAccessVerifier(cfg.Access.AccessConfig)
		}
		if cfg.Access.Scopes != nil {
			accessScopes = cfg.Access.Scopes
		}
	}
	authn, err := auth.New(cfg.Tokens, verifier, accessScopes, h.KindNames())
	if err != nil {
		return nil, err
	}

	a := &app{store: st, hub: h, dispatcher: disp}
	for _, b := range bots {
		b.EnableCommands(st, h.KindNames)
		a.background = append(a.background, b.RunCommands)
	}
	a.server = &http.Server{
		Addr:              cfg.Listen,
		Handler:           api.New(h, st, authn, infos, web.FS(), log).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	log.Info("configured", "kinds", h.KindNames(), "senders", names, "tokens", len(cfg.Tokens), "access", verifier != nil)
	return a, nil
}

func run(cfgPath string, log *slog.Logger) error {
	a, err := build(cfgPath, log, false)
	if err != nil {
		return err
	}
	defer a.store.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go a.dispatcher.Run(ctx)
	for _, f := range a.background {
		go f(ctx)
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.server.Shutdown(sctx)
	}()

	log.Info("msg starting", "version", version, "listen", a.server.Addr)
	if err := a.server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
