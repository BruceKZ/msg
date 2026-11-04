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
	"sort"
	"syscall"
	"time"
	_ "time/tzdata" // OpenWrt ships no zoneinfo database

	"msg/internal/api"
	"msg/internal/auth"
	"msg/internal/core"
	"msg/internal/dispatch"
	"msg/internal/hub"
	"msg/internal/kinds/sms"
	"msg/internal/senders"
	"msg/internal/senders/discord"
	"msg/internal/senders/telegram"
	"msg/internal/store"
	"msg/web"
)

var version = "dev"

// availableKinds lists every kind compiled into the binary.
func availableKinds() []core.Kind {
	return []core.Kind{sms.New()}
}

// senderTypes lists every sender type compiled into the binary. Sender
// instances themselves are configured at runtime (web settings / API).
func senderTypes() []senders.Type {
	return []senders.Type{telegram.Type(), discord.Type()}
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
		if _, err := build(context.Background(), *cfgPath, log, true); err != nil {
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
}

// build assembles everything from the config. With dryRun the database is
// opened in memory so the real one is not touched. Background work of
// senders stops when ctx is cancelled.
func build(ctx context.Context, cfgPath string, log *slog.Logger, dryRun bool) (*app, error) {
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

	sort.Slice(kinds, func(i, j int) bool { return kinds[i].Info().Name < kinds[j].Info().Name })
	kindNames := make([]string, len(kinds))
	for i, k := range kinds {
		kindNames[i] = k.Info().Name
	}

	// Senders are configured at runtime and stored in the database.
	mgr, err := senders.New(ctx, st, senderTypes(), senders.Env{
		Loc: loc, Log: log, Inbox: st, KindNames: func() []string { return slices.Clone(kindNames) },
	})
	if err != nil {
		return nil, fmt.Errorf("senders: %w", err)
	}
	disp := dispatch.New(st, mgr, log)
	h, err := hub.New(st, kinds, mgr, hub.Options{DedupWindows: windows, Wake: disp.Wake})
	if err != nil {
		return nil, err
	}

	var verifier *auth.AccessVerifier
	accessScopes := []string{auth.ScopeMessagesRead, auth.ScopeMessagesDelete, auth.ScopeDeliveriesWrite, auth.ScopeSendersWrite}
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
	a.server = &http.Server{
		Addr:              cfg.Listen,
		Handler:           api.New(h, st, authn, mgr, web.FS(), log).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	log.Info("configured", "kinds", h.KindNames(), "senders", mgr.ActiveNames(), "tokens", len(cfg.Tokens), "access", verifier != nil)
	return a, nil
}

func run(cfgPath string, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	a, err := build(ctx, cfgPath, log, false)
	if err != nil {
		return err
	}
	defer a.store.Close()
	go a.dispatcher.Run(ctx)
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
