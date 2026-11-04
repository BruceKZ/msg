// Package auth identifies callers (bearer tokens or Cloudflare Access) and
// decides what they may do (scopes, kinds).
package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
)

const (
	ScopeMessagesWrite   = "messages:write"
	ScopeMessagesRead    = "messages:read"
	ScopeMessagesDelete  = "messages:delete"
	ScopeDeliveriesWrite = "deliveries:write"
	ScopeSendersWrite    = "senders:write" // manage senders (incl. their secrets) and routes
)

var AllScopes = []string{ScopeMessagesWrite, ScopeMessagesRead, ScopeMessagesDelete, ScopeDeliveriesWrite, ScopeSendersWrite}

// Principal is an authenticated caller.
type Principal struct {
	Name   string   `json:"name"` // token name, or the Access user's email
	Via    string   `json:"via"`  // "token" | "access"
	Scopes []string `json:"scopes"`
	Kinds  []string `json:"kinds"` // nil = all kinds
}

func (p *Principal) Has(scope string) bool { return slices.Contains(p.Scopes, scope) }

func (p *Principal) CanKind(kind string) bool {
	return p.Kinds == nil || slices.Contains(p.Kinds, kind)
}

// FilterKinds narrows requested kinds (nil = all known kinds) to those the principal may access.
func (p *Principal) FilterKinds(requested, known []string) []string {
	if requested == nil {
		if p.Kinds == nil {
			return nil
		}
		requested = known
	}
	out := []string{}
	for _, k := range requested {
		if p.CanKind(k) && !slices.Contains(out, k) {
			out = append(out, k)
		}
	}
	return out
}

type Token struct {
	Name   string   `json:"name"`
	Token  string   `json:"token"`
	Scopes []string `json:"scopes"`
	Kinds  []string `json:"kinds"`
}

type Authenticator struct {
	tokens       []tokenEntry
	access       *AccessVerifier
	accessScopes []string
}

type tokenEntry struct {
	hash [32]byte
	p    Principal
}

var (
	ErrNoCredentials = errors.New("no credentials")
	ErrBadToken      = errors.New("invalid token")
)

// New validates the token list. knownKinds is used to reject typos in token kind lists.
func New(tokens []Token, access *AccessVerifier, accessScopes []string, knownKinds []string) (*Authenticator, error) {
	a := &Authenticator{access: access, accessScopes: accessScopes}
	names := map[string]bool{}
	secrets := map[string]bool{}
	for i, t := range tokens {
		switch {
		case t.Name == "":
			return nil, fmt.Errorf("tokens[%d]: name is required", i)
		case names[t.Name]:
			return nil, fmt.Errorf("tokens[%d]: duplicate name %q", i, t.Name)
		case len(t.Token) < 16:
			return nil, fmt.Errorf("tokens[%d] (%s): token must be at least 16 characters", i, t.Name)
		case secrets[t.Token]:
			return nil, fmt.Errorf("tokens[%d] (%s): token reused", i, t.Name)
		case len(t.Scopes) == 0:
			return nil, fmt.Errorf("tokens[%d] (%s): no scopes", i, t.Name)
		}
		if err := checkScopes(t.Scopes); err != nil {
			return nil, fmt.Errorf("tokens[%d] (%s): %w", i, t.Name, err)
		}
		for _, k := range t.Kinds {
			if !slices.Contains(knownKinds, k) {
				return nil, fmt.Errorf("tokens[%d] (%s): unknown kind %q", i, t.Name, k)
			}
		}
		names[t.Name], secrets[t.Token] = true, true
		var kinds []string
		if t.Kinds != nil {
			kinds = slices.Clone(t.Kinds)
		}
		a.tokens = append(a.tokens, tokenEntry{
			hash: sha256.Sum256([]byte(t.Token)),
			p:    Principal{Name: t.Name, Via: "token", Scopes: slices.Clone(t.Scopes), Kinds: kinds},
		})
	}
	if err := checkScopes(accessScopes); err != nil {
		return nil, fmt.Errorf("access.scopes: %w", err)
	}
	return a, nil
}

func checkScopes(scopes []string) error {
	for _, s := range scopes {
		if !slices.Contains(AllScopes, s) {
			return fmt.Errorf("unknown scope %q", s)
		}
	}
	return nil
}

// Authenticate returns the caller, ErrNoCredentials if the request carries
// none, or another error if the credentials are invalid.
func (a *Authenticator) Authenticate(r *http.Request) (*Principal, error) {
	if tok, ok := bearer(r); ok {
		h := sha256.Sum256([]byte(tok))
		for i := range a.tokens {
			if subtle.ConstantTimeCompare(h[:], a.tokens[i].hash[:]) == 1 {
				p := a.tokens[i].p
				return &p, nil
			}
		}
		return nil, ErrBadToken
	}
	if a.access != nil {
		jwt := r.Header.Get("Cf-Access-Jwt-Assertion")
		if jwt == "" {
			// On paths where Access is bypassed, Cloudflare adds no header, but the
			// browser still sends the Access session cookie, which holds the same signed JWT.
			if c, err := r.Cookie("CF_Authorization"); err == nil && c.Value != "" {
				if !sameOrigin(r) {
					return nil, ErrCrossSite
				}
				jwt = c.Value
			}
		}
		if jwt != "" {
			id, err := a.access.Verify(r.Context(), jwt)
			if err != nil {
				return nil, fmt.Errorf("access: %w", err)
			}
			return &Principal{Name: id.Email, Via: "access", Scopes: slices.Clone(a.accessScopes)}, nil
		}
	}
	return nil, ErrNoCredentials
}

var ErrCrossSite = errors.New("cross-site request with session cookie")

// sameOrigin guards cookie-authenticated requests against CSRF: state-changing
// requests must come from this site. Safe methods are always allowed.
func sameOrigin(r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin":
		return true
	case "":
		// Older browsers: fall back to Origin, which must match the host.
		o := r.Header.Get("Origin")
		return o != "" && (o == "https://"+r.Host || o == "http://"+r.Host)
	default:
		return false
	}
}

func bearer(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:]), true
	}
	return "", false
}
