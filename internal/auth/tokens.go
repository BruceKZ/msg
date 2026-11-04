package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"regexp"
	"slices"
	"time"

	"msg/internal/core"
	"msg/internal/store"
)

// MintableScopes are the scopes a token created in the web UI may carry.
// Managing senders and tokens stays with logged-in people (Cloudflare Access),
// so a leaked token can never be used to create more tokens or redirect notifications.
var MintableScopes = []string{ScopeMessagesWrite, ScopeMessagesRead, ScopeMessagesDelete, ScopeDeliveriesWrite}

const tokenPrefix = "msg_"

var tokenNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// UseStore enables tokens created and revoked at runtime.
func (a *Authenticator) UseStore(st *store.Store) { a.db = st }

func hashHex(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:])
}

// TokenInfo describes a token without its secret.
type TokenInfo struct {
	ID         int64      `json:"id"` // 0 for config-file tokens
	Name       string     `json:"name"`
	Origin     string     `json:"origin"` // "web" | "config"
	Prefix     string     `json:"prefix,omitempty"`
	Scopes     []string   `json:"scopes"`
	Kinds      []string   `json:"kinds"` // null = all kinds
	CreatedAt  *time.Time `json:"created_at,omitempty"`
	CreatedBy  string     `json:"created_by,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

func infoFromRow(r store.TokenRow) TokenInfo {
	t := TokenInfo{ID: r.ID, Name: r.Name, Origin: "web", Prefix: r.Prefix, Scopes: r.Scopes, Kinds: r.Kinds, CreatedBy: r.CreatedBy}
	t.CreatedAt = &r.CreatedAt
	if !r.LastUsedAt.IsZero() {
		t.LastUsedAt = &r.LastUsedAt
	}
	if !r.RevokedAt.IsZero() {
		t.RevokedAt = &r.RevokedAt
	}
	return t
}

// ListTokens returns config-file tokens followed by web tokens (newest first).
func (a *Authenticator) ListTokens(ctx context.Context, includeRevoked bool) ([]TokenInfo, error) {
	out := []TokenInfo{}
	for _, t := range a.tokens {
		out = append(out, TokenInfo{Name: t.p.Name, Origin: "config", Scopes: t.p.Scopes, Kinds: t.p.Kinds})
	}
	if a.db == nil {
		return out, nil
	}
	rows, err := a.db.Tokens(ctx, includeRevoked)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out = append(out, infoFromRow(r))
	}
	return out, nil
}

type NewToken struct {
	Name   string   `json:"name"`
	Scopes []string `json:"scopes"`
	Kinds  []string `json:"kinds"` // null = all kinds
}

// CreateToken mints a token. The returned secret is shown to the caller once and never stored.
func (a *Authenticator) CreateToken(ctx context.Context, nt NewToken, by string) (string, TokenInfo, error) {
	if a.db == nil {
		return "", TokenInfo{}, errors.New("token storage not configured")
	}
	if !tokenNameRe.MatchString(nt.Name) {
		return "", TokenInfo{}, &core.ValidationError{Field: "name", Message: "must be 1–64 characters: lowercase letters, digits, '.', '-' or '_'"}
	}
	for _, t := range a.tokens {
		if t.p.Name == nt.Name {
			return "", TokenInfo{}, &core.ValidationError{Field: "name", Message: "already used by a token in the config file"}
		}
	}
	if len(nt.Scopes) == 0 {
		return "", TokenInfo{}, &core.ValidationError{Field: "scopes", Message: "pick at least one scope"}
	}
	scopes := slices.Compact(slices.Sorted(slices.Values(nt.Scopes)))
	for _, s := range scopes {
		if !slices.Contains(MintableScopes, s) {
			return "", TokenInfo{}, &core.ValidationError{Field: "scopes", Message: "scope " + s + " cannot be given to a token"}
		}
	}
	var kinds []string
	if nt.Kinds != nil {
		kinds = append([]string{}, slices.Compact(slices.Sorted(slices.Values(nt.Kinds)))...)
		if len(kinds) == 0 {
			return "", TokenInfo{}, &core.ValidationError{Field: "kinds", Message: "pick at least one kind, or null for all"}
		}
		for _, k := range kinds {
			if !slices.Contains(a.knownKinds, k) {
				return "", TokenInfo{}, &core.ValidationError{Field: "kinds", Message: "unknown kind " + k}
			}
		}
	}

	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", TokenInfo{}, err
	}
	secret := tokenPrefix + base64.RawURLEncoding.EncodeToString(b)
	row := store.TokenRow{Name: nt.Name, Hash: hashHex(secret), Prefix: secret[:len(tokenPrefix)+6], Scopes: scopes, Kinds: kinds, CreatedBy: by}
	id, err := a.db.CreateToken(ctx, row)
	if errors.Is(err, store.ErrExists) {
		return "", TokenInfo{}, &core.ValidationError{Field: "name", Message: "an active token with this name already exists"}
	}
	if err != nil {
		return "", TokenInfo{}, err
	}
	row.ID, row.CreatedAt = id, time.Now().UTC()
	return secret, infoFromRow(row), nil
}

// RevokeToken disables a web token immediately.
func (a *Authenticator) RevokeToken(ctx context.Context, id int64) (TokenInfo, error) {
	if a.db == nil {
		return TokenInfo{}, store.ErrNotFound
	}
	r, err := a.db.RevokeToken(ctx, id)
	if err != nil {
		return TokenInfo{}, err
	}
	return infoFromRow(r), nil
}
