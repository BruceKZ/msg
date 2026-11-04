// Package api serves the HTTP API (/api/v1) and the web UI.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"msg/internal/auth"
	"msg/internal/core"
	"msg/internal/hub"
	"msg/internal/senders"
	"msg/internal/store"
)

const maxBody = 64 << 10

type Server struct {
	hub     *hub.Hub
	store   *store.Store
	auth    *auth.Authenticator
	senders *senders.Manager
	web     fs.FS
	log     *slog.Logger
}

func New(h *hub.Hub, st *store.Store, a *auth.Authenticator, mgr *senders.Manager, web fs.FS, log *slog.Logger) *Server {
	return &Server{hub: h, store: st, auth: a, senders: mgr, web: web, log: log}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	route := func(pattern, scope string, h handler) { mux.Handle(pattern, s.authed(scope, h)) }

	route("POST /api/v1/messages", auth.ScopeMessagesWrite, s.createMessage)
	route("GET /api/v1/messages", auth.ScopeMessagesRead, s.listMessages)
	route("GET /api/v1/messages/{id}", auth.ScopeMessagesRead, s.getMessage)
	route("DELETE /api/v1/messages/{id}", auth.ScopeMessagesDelete, s.deleteMessage)
	route("GET /api/v1/messages/{id}/deliveries", auth.ScopeMessagesRead, s.listDeliveries)
	route("POST /api/v1/messages/{id}/deliveries", auth.ScopeDeliveriesWrite, s.redeliver)
	route("GET /api/v1/kinds", "", s.listKinds)
	route("GET /api/v1/senders", "", s.listSenders)
	route("POST /api/v1/senders", auth.ScopeSendersWrite, s.createSender)
	route("GET /api/v1/senders/{name}", auth.ScopeSendersWrite, s.getSender)
	route("PATCH /api/v1/senders/{name}", auth.ScopeSendersWrite, s.updateSender)
	route("DELETE /api/v1/senders/{name}", auth.ScopeSendersWrite, s.deleteSender)
	route("POST /api/v1/senders/{name}/test", auth.ScopeSendersWrite, s.testSender)
	route("GET /api/v1/senders/{name}/chats", auth.ScopeSendersWrite, s.senderChats)
	route("GET /api/v1/sender-types", auth.ScopeSendersWrite, s.senderTypes)
	route("GET /api/v1/routes", "", s.listRoutes)
	route("PUT /api/v1/routes/{kind}", auth.ScopeSendersWrite, s.setRoute)
	route("GET /api/v1/tokens", auth.ScopeTokensWrite, s.listTokens)
	route("POST /api/v1/tokens", auth.ScopeTokensWrite, s.createToken)
	route("DELETE /api/v1/tokens/{id}", auth.ScopeTokensWrite, s.revokeToken)
	route("GET /api/v1/me", "", s.me)

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "not_found", "no such endpoint", "")
	})
	if s.web != nil {
		files := http.FileServerFS(s.web)
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				w.Header().Set("Allow", "GET, HEAD")
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			files.ServeHTTP(w, r)
		})
	}
	return headers(mux)
}

func headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
		} else {
			// The UI is embedded in the binary; make browsers fetch the new one after an upgrade.
			h.Set("Cache-Control", "no-cache")
		}
		next.ServeHTTP(w, r)
	})
}

// ---- auth ----

type handler func(w http.ResponseWriter, r *http.Request, p *auth.Principal)

// authed requires a valid identity, and scope unless it is empty.
func (s *Server) authed(scope string, h handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := s.auth.Authenticate(r)
		if err != nil {
			if !errors.Is(err, auth.ErrNoCredentials) {
				s.log.Warn("authentication failed", "path", r.URL.Path, "err", err)
			}
			writeErr(w, http.StatusUnauthorized, "unauthorized", "missing or invalid credentials", "")
			return
		}
		if scope != "" && !p.Has(scope) {
			writeErr(w, http.StatusForbidden, "forbidden", "requires scope "+scope, "")
			return
		}
		h(w, r, p)
	})
}

// ---- messages ----

type createReq struct {
	Kind     string          `json:"kind"`
	Data     json.RawMessage `json:"data"`
	Senders  []string        `json:"senders"`
	DedupKey string          `json:"dedup_key"`
}

func (s *Server) createMessage(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	req, ok := s.decodeCreate(w, r)
	if !ok {
		return
	}
	if req.Kind == "" {
		writeErr(w, http.StatusBadRequest, "validation_failed", "kind is required", "kind")
		return
	}
	if len(req.Data) == 0 || string(req.Data) == "null" {
		writeErr(w, http.StatusBadRequest, "validation_failed", "data is required", "data")
		return
	}
	if !p.CanKind(req.Kind) {
		writeErr(w, http.StatusForbidden, "forbidden", "token may not submit kind "+req.Kind, "kind")
		return
	}

	m, dup, err := s.hub.Submit(r.Context(), hub.Submission{
		Kind: req.Kind, Data: req.Data, Source: p.Name, Senders: req.Senders, DedupKey: req.DedupKey,
	})
	var ve *core.ValidationError
	switch {
	case errors.As(err, &ve):
		writeErr(w, http.StatusBadRequest, "validation_failed", ve.Message, ve.Field)
		return
	case errors.Is(err, core.ErrUnknownKind):
		writeErr(w, http.StatusBadRequest, "unknown_kind", err.Error(), "kind")
		return
	case errors.Is(err, core.ErrUnknownSender):
		writeErr(w, http.StatusBadRequest, "unknown_sender", err.Error(), "senders")
		return
	case err != nil:
		s.internal(w, "submit", err)
		return
	}

	if dup {
		s.log.Info("duplicate ignored", "id", m.ID, "kind", m.Kind, "source", m.Source)
		w.Header().Set("X-Duplicate", "true")
		writeJSON(w, http.StatusOK, m)
		return
	}
	s.log.Info("message stored", "id", m.ID, "kind", m.Kind, "source", m.Source)
	w.Header().Set("Location", "/api/v1/messages/"+strconv.FormatInt(m.ID, 10))
	writeJSON(w, http.StatusCreated, m)
}

// decodeCreate reads a JSON body, or a form where every field except kind,
// senders and dedup_key becomes a string field of data.
func (s *Server) decodeCreate(w http.ResponseWriter, r *http.Request) (createReq, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	var req createReq
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct == "application/x-www-form-urlencoded" || ct == "multipart/form-data" {
		var err error
		if ct == "multipart/form-data" {
			err = r.ParseMultipartForm(maxBody)
		} else {
			err = r.ParseForm()
		}
		if err != nil {
			return req, s.bodyErr(w, err)
		}
		data := map[string]string{}
		for k, v := range r.PostForm {
			switch k {
			case "kind":
				req.Kind = v[0]
			case "dedup_key":
				req.DedupKey = v[0]
			case "senders":
				for _, part := range v {
					for _, name := range strings.Split(part, ",") {
						if name = strings.TrimSpace(name); name != "" {
							req.Senders = append(req.Senders, name)
						}
					}
				}
				if req.Senders == nil {
					req.Senders = []string{}
				}
			default:
				data[k] = v[0]
			}
		}
		req.Data, _ = json.Marshal(data)
		return req, true
	}

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return req, s.bodyErr(w, err)
	}
	return req, true
}

func (s *Server) bodyErr(w http.ResponseWriter, err error) bool {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		writeErr(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body exceeds 64 KB", "")
	} else {
		writeErr(w, http.StatusBadRequest, "invalid_json", err.Error(), "")
	}
	return false
}

func (s *Server) listMessages(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	q := r.URL.Query()
	f := store.Filter{Source: q.Get("source"), Query: q.Get("q")}
	var err error
	if f.Before, err = intParam(q, "before"); err != nil {
		writeErr(w, http.StatusBadRequest, "validation_failed", err.Error(), "before")
		return
	}
	// "since" is the older name of "after".
	for _, name := range []string{"after", "since"} {
		if q.Has(name) && f.After == 0 {
			if f.After, err = intParam(q, name); err != nil {
				writeErr(w, http.StatusBadRequest, "validation_failed", err.Error(), name)
				return
			}
		}
	}
	switch q.Get("order") {
	case "", "desc":
	case "asc":
		f.Asc = true
	default:
		writeErr(w, http.StatusBadRequest, "validation_failed", "order must be asc or desc", "order")
		return
	}
	limit, err := intParam(q, "limit")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation_failed", err.Error(), "limit")
		return
	}
	f.Limit = int(limit)
	if f.Limit == 0 {
		f.Limit = store.DefaultLimit
	}
	f.Limit = min(f.Limit, store.MaxLimit)

	var requested []string
	if ks, ok := q["kind"]; ok {
		for _, k := range ks {
			for _, part := range strings.Split(k, ",") {
				if part = strings.TrimSpace(part); part != "" {
					requested = append(requested, part)
				}
			}
		}
		if requested == nil {
			requested = []string{}
		}
	}
	f.Kinds = p.FilterKinds(requested, s.hub.KindNames())

	items, err := s.store.List(r.Context(), f)
	if err != nil {
		s.internal(w, "list", err)
		return
	}
	// A full page means there may be more: hand back the cursor for the next page
	// in the same direction (next_before when newest-first, next_after when oldest-first).
	var nextBefore, nextAfter *int64
	if len(items) == f.Limit && len(items) > 0 {
		id := items[len(items)-1].ID
		if f.Asc {
			nextAfter = &id
		} else {
			nextBefore = &id
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next_before": nextBefore, "next_after": nextAfter})
}

func intParam(q url.Values, name string) (int64, error) {
	v := q.Get(name)
	if v == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, errors.New(name + " must be a non-negative integer")
	}
	return n, nil
}

// loadMessage fetches {id} and hides it (404) if the principal may not see its kind.
func (s *Server) loadMessage(w http.ResponseWriter, r *http.Request, p *auth.Principal) (store.Message, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusNotFound, "not_found", "message not found", "")
		return store.Message{}, false
	}
	m, err := s.store.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && !p.CanKind(m.Kind)) {
		writeErr(w, http.StatusNotFound, "not_found", "message not found", "")
		return store.Message{}, false
	}
	if err != nil {
		s.internal(w, "get", err)
		return store.Message{}, false
	}
	return m, true
}

func (s *Server) getMessage(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	if m, ok := s.loadMessage(w, r, p); ok {
		writeJSON(w, http.StatusOK, m)
	}
}

func (s *Server) deleteMessage(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	m, ok := s.loadMessage(w, r, p)
	if !ok {
		return
	}
	if err := s.store.Delete(r.Context(), m.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.internal(w, "delete", err)
		return
	}
	s.log.Info("message deleted", "id", m.ID, "by", p.Name)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listDeliveries(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	m, ok := s.loadMessage(w, r, p)
	if !ok {
		return
	}
	ds, err := s.store.Deliveries(r.Context(), m.ID)
	if err != nil {
		s.internal(w, "deliveries", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": ds})
}

func (s *Server) redeliver(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	m, ok := s.loadMessage(w, r, p)
	if !ok {
		return
	}
	var req struct {
		Senders []string `json:"senders"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		s.bodyErr(w, err)
		return
	}
	for _, name := range req.Senders {
		if !s.hub.HasSender(name) {
			writeErr(w, http.StatusBadRequest, "unknown_sender", "unknown sender "+strconv.Quote(name), "senders")
			return
		}
	}
	n, err := s.store.Redeliver(r.Context(), m.ID, req.Senders)
	if err != nil {
		s.internal(w, "redeliver", err)
		return
	}
	if n > 0 {
		s.hub.Wake()
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"queued": n})
}

// ---- API tokens ----

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	items, err := s.auth.ListTokens(r.Context(), r.URL.Query().Get("include_revoked") == "1")
	if err != nil {
		s.internal(w, "list tokens", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "mintable_scopes": auth.MintableScopes, "kinds": s.hub.KindNames()})
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	var req auth.NewToken
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.bodyErr(w, err)
		return
	}
	secret, info, err := s.auth.CreateToken(r.Context(), req, p.Name)
	var ve *core.ValidationError
	switch {
	case errors.As(err, &ve):
		writeErr(w, http.StatusBadRequest, "validation_failed", ve.Message, ve.Field)
		return
	case err != nil:
		s.internal(w, "create token", err)
		return
	}
	s.log.Info("token created", "id", info.ID, "name", info.Name, "scopes", info.Scopes, "by", p.Name)
	w.Header().Set("Location", "/api/v1/tokens/"+strconv.FormatInt(info.ID, 10))
	// The secret is returned exactly once; only its hash is stored.
	writeJSON(w, http.StatusCreated, map[string]any{"token": secret, "info": info})
}

func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusNotFound, "not_found", "token not found", "")
		return
	}
	info, err := s.auth.RevokeToken(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found", "token not found or already revoked", "")
		return
	}
	if err != nil {
		s.internal(w, "revoke token", err)
		return
	}
	s.log.Info("token revoked", "id", id, "name", info.Name, "by", p.Name)
	writeJSON(w, http.StatusOK, info)
}

// ---- metadata ----

func (s *Server) listKinds(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	items := []hub.KindInfo{}
	for _, k := range s.hub.Kinds() {
		if p.CanKind(k.Name) {
			items = append(items, k)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// ---- senders & routes ----

// listSenders shows every caller the senders' names and state; configuration
// (never secrets) only to those who may manage senders.
func (s *Server) listSenders(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	writeJSON(w, http.StatusOK, map[string]any{"items": s.senders.List(p.Has(auth.ScopeSendersWrite))})
}

func (s *Server) senderTypes(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	writeJSON(w, http.StatusOK, map[string]any{"items": s.senders.Types()})
}

func (s *Server) decodeChange(w http.ResponseWriter, r *http.Request) (senders.Change, bool) {
	var c senders.Change
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, s.bodyErr(w, err)
	}
	return c, true
}

func (s *Server) senderErr(w http.ResponseWriter, op string, err error) {
	var ve *core.ValidationError
	switch {
	case errors.As(err, &ve):
		writeErr(w, http.StatusBadRequest, "validation_failed", ve.Message, ve.Field)
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", "sender not found", "")
	case errors.Is(err, store.ErrExists):
		writeErr(w, http.StatusConflict, "conflict", "a sender with this name already exists", "name")
	case errors.Is(err, senders.ErrNotActive):
		writeErr(w, http.StatusConflict, "sender_inactive", "sender is disabled or failed to start", "")
	case errors.Is(err, senders.ErrNoChats):
		writeErr(w, http.StatusBadRequest, "unsupported", err.Error(), "")
	default:
		s.internal(w, op, err)
	}
}

func (s *Server) createSender(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	c, ok := s.decodeChange(w, r)
	if !ok {
		return
	}
	info, err := s.senders.Create(r.Context(), c)
	if err != nil {
		s.senderErr(w, "create sender", err)
		return
	}
	s.log.Info("sender created", "sender", info.Name, "type", info.Type, "by", p.Name)
	w.Header().Set("Location", "/api/v1/senders/"+url.PathEscape(info.Name))
	writeJSON(w, http.StatusCreated, info)
}

func (s *Server) getSender(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	info, err := s.senders.Get(r.PathValue("name"), true)
	if err != nil {
		s.senderErr(w, "get sender", err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) updateSender(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	c, ok := s.decodeChange(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	if c.Name != "" && c.Name != name {
		writeErr(w, http.StatusBadRequest, "validation_failed", "name cannot be changed", "name")
		return
	}
	info, err := s.senders.Update(r.Context(), name, c)
	if err != nil {
		s.senderErr(w, "update sender", err)
		return
	}
	s.log.Info("sender updated", "sender", name, "enabled", info.Enabled, "by", p.Name)
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) deleteSender(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	name := r.PathValue("name")
	if err := s.senders.Delete(r.Context(), name); err != nil {
		s.senderErr(w, "delete sender", err)
		return
	}
	s.log.Info("sender deleted", "sender", name, "by", p.Name)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) testSender(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	err := s.senders.Test(r.Context(), r.PathValue("name"), p.Name)
	var ve *core.ValidationError
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case errors.Is(err, store.ErrNotFound), errors.Is(err, senders.ErrNotActive), errors.As(err, &ve):
		s.senderErr(w, "test sender", err)
	default:
		// The send itself failed: report the sender's error to the caller.
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
	}
}

func (s *Server) senderChats(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	chats, err := s.senders.Chats(r.PathValue("name"))
	if err != nil {
		s.senderErr(w, "sender chats", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": chats})
}

func (s *Server) listRoutes(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	routes := s.senders.Routes()
	items := []map[string]any{}
	for _, kind := range s.hub.KindNames() {
		if !p.CanKind(kind) {
			continue
		}
		list, custom := routes[kind]
		items = append(items, map[string]any{
			"kind": kind, "custom": custom, "senders": list, "effective": s.senders.DefaultSenders(kind),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) setRoute(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	kind := r.PathValue("kind")
	if !s.hub.HasKind(kind) {
		writeErr(w, http.StatusNotFound, "not_found", "unknown kind "+strconv.Quote(kind), "")
		return
	}
	var req struct {
		Senders *[]string `json:"senders"` // null = every sender (default)
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.bodyErr(w, err)
		return
	}
	var list []string
	if req.Senders != nil {
		list = append([]string{}, *req.Senders...)
	}
	if err := s.senders.SetRoute(r.Context(), kind, list); err != nil {
		s.senderErr(w, "set route", err)
		return
	}
	s.log.Info("route set", "kind", kind, "senders", list, "by", p.Name)
	writeJSON(w, http.StatusOK, map[string]any{"kind": kind, "custom": list != nil, "senders": list, "effective": s.senders.DefaultSenders(kind)})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	writeJSON(w, http.StatusOK, p)
}

// ---- responses ----

func (s *Server) internal(w http.ResponseWriter, op string, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	s.log.Error(op, "err", err)
	writeErr(w, http.StatusInternalServerError, "internal", "internal error", "")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg, field string) {
	e := map[string]string{"code": code, "message": msg}
	if field != "" {
		e["field"] = field
	}
	writeJSON(w, status, map[string]any{"error": e})
}
