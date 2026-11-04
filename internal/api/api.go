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
	"msg/internal/store"
)

const maxBody = 64 << 10

// SenderInfo describes a configured sender for GET /api/v1/senders (no secrets).
type SenderInfo struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Enabled bool   `json:"enabled"`
}

type Server struct {
	hub     *hub.Hub
	store   *store.Store
	auth    *auth.Authenticator
	senders []SenderInfo
	web     fs.FS
	log     *slog.Logger
}

func New(h *hub.Hub, st *store.Store, a *auth.Authenticator, senders []SenderInfo, web fs.FS, log *slog.Logger) *Server {
	return &Server{hub: h, store: st, auth: a, senders: senders, web: web, log: log}
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
	if f.Since, err = intParam(q, "since"); err != nil {
		writeErr(w, http.StatusBadRequest, "validation_failed", err.Error(), "since")
		return
	}
	limit, err := intParam(q, "limit")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation_failed", err.Error(), "limit")
		return
	}
	f.Limit = int(limit)
	if f.Limit == 0 {
		f.Limit = 50
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
	var next *int64
	if len(items) == f.Limit && len(items) > 0 {
		id := items[len(items)-1].ID
		next = &id
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next_before": next})
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

func (s *Server) listSenders(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	writeJSON(w, http.StatusOK, map[string]any{"items": s.senders})
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
