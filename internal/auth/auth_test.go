package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func sign(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	hdr, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": kid})
	body, _ := json.Marshal(claims)
	input := b64(hdr) + "." + b64(body)
	sum := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return input + "." + b64(sig)
}

func TestAccessVerify(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	certs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kid": "k1", "kty": "RSA", "n": b64(key.N.Bytes()), "e": b64(big.NewInt(int64(key.E)).Bytes()),
		}}})
	}))
	defer certs.Close()

	v := NewAccessVerifier(AccessConfig{TeamDomain: "team.cloudflareaccess.com", AUD: "aud123"})
	v.certURL = certs.URL
	good := map[string]any{
		"aud": []string{"aud123"}, "iss": "https://team.cloudflareaccess.com",
		"exp": time.Now().Add(time.Hour).Unix(), "email": "me@example.com",
	}
	id, err := v.Verify(context.Background(), sign(t, key, "k1", good))
	if err != nil || id.Email != "me@example.com" {
		t.Fatalf("good token: %v %v", id, err)
	}

	with := func(k string, val any) map[string]any {
		c := map[string]any{}
		for kk, vv := range good {
			c[kk] = vv
		}
		c[k] = val
		return c
	}
	bad := map[string]string{
		"wrong aud":    sign(t, key, "k1", with("aud", "other")),
		"wrong issuer": sign(t, key, "k1", with("iss", "https://evil.cloudflareaccess.com")),
		"expired":      sign(t, key, "k1", with("exp", time.Now().Add(-time.Hour).Unix())),
		"wrong key":    sign(t, other, "k1", good),
		"unknown kid":  sign(t, key, "k2", good),
		"garbage":      "a.b.c",
	}
	tampered := sign(t, key, "k1", good)
	parts := strings.Split(tampered, ".")
	evil, _ := json.Marshal(with("email", "attacker@example.com"))
	bad["tampered payload"] = parts[0] + "." + b64(evil) + "." + parts[2]
	for name, tok := range bad {
		if _, err := v.Verify(context.Background(), tok); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestAuthenticate(t *testing.T) {
	a, err := New([]Token{{Name: "phone", Token: "phone-token-0123456789", Scopes: []string{ScopeMessagesWrite}, Kinds: []string{"sms"}}},
		nil, []string{ScopeMessagesRead}, []string{"sms"})
	if err != nil {
		t.Fatal(err)
	}
	req := func(h string) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		if h != "" {
			r.Header.Set("Authorization", h)
		}
		return r
	}
	if p, err := a.Authenticate(req("Bearer phone-token-0123456789")); err != nil || p.Name != "phone" || !p.CanKind("sms") || p.CanKind("x") {
		t.Fatalf("token: %+v %v", p, err)
	}
	if _, err := a.Authenticate(req("bearer phone-token-0123456789")); err != nil {
		t.Fatal("scheme should be case-insensitive")
	}
	if _, err := a.Authenticate(req("Bearer wrong")); err != ErrBadToken {
		t.Fatalf("wrong token: %v", err)
	}
	if _, err := a.Authenticate(req("")); err != ErrNoCredentials {
		t.Fatalf("no credentials: %v", err)
	}
	// A JWT header is ignored when Access is not configured.
	r := req("")
	r.Header.Set("Cf-Access-Jwt-Assertion", "x.y.z")
	if _, err := a.Authenticate(r); err != ErrNoCredentials {
		t.Fatalf("access disabled: %v", err)
	}
}

func TestNewRejectsBadTokens(t *testing.T) {
	ok := Token{Name: "a", Token: "aaaaaaaaaaaaaaaaaaaa", Scopes: []string{ScopeMessagesRead}}
	cases := map[string][]Token{
		"short":         {{Name: "a", Token: "short", Scopes: []string{ScopeMessagesRead}}},
		"no name":       {{Token: "aaaaaaaaaaaaaaaaaaaa", Scopes: []string{ScopeMessagesRead}}},
		"no scopes":     {{Name: "a", Token: "aaaaaaaaaaaaaaaaaaaa"}},
		"bad scope":     {{Name: "a", Token: "aaaaaaaaaaaaaaaaaaaa", Scopes: []string{"admin"}}},
		"unknown kind":  {{Name: "a", Token: "aaaaaaaaaaaaaaaaaaaa", Scopes: []string{ScopeMessagesRead}, Kinds: []string{"fax"}}},
		"dup name":      {ok, {Name: "a", Token: "bbbbbbbbbbbbbbbbbbbb", Scopes: []string{ScopeMessagesRead}}},
		"reused secret": {ok, {Name: "b", Token: "aaaaaaaaaaaaaaaaaaaa", Scopes: []string{ScopeMessagesRead}}},
	}
	for name, toks := range cases {
		if _, err := New(toks, nil, nil, []string{"sms"}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestFilterKinds(t *testing.T) {
	all := &Principal{}
	some := &Principal{Kinds: []string{"sms"}}
	known := []string{"note", "sms"}
	if all.FilterKinds(nil, known) != nil {
		t.Error("unrestricted, no request: want nil (all)")
	}
	if got := some.FilterKinds(nil, known); len(got) != 1 || got[0] != "sms" {
		t.Errorf("restricted: %v", got)
	}
	if got := some.FilterKinds([]string{"note"}, known); got == nil || len(got) != 0 {
		t.Errorf("disallowed request: want empty, got %v", got)
	}
}
