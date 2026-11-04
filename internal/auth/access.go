// Package auth verifies Cloudflare Access JWTs (Cf-Access-Jwt-Assertion).
package auth

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

type AccessConfig struct {
	TeamDomain string `json:"team_domain"` // e.g. "bruce.cloudflareaccess.com"
	AUD        string `json:"aud"`         // Application Audience (AUD) tag
}

type AccessVerifier struct {
	cfg     AccessConfig
	certURL string
	issuer  string
	client  *http.Client

	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	fetched time.Time
}

func NewAccessVerifier(cfg AccessConfig) *AccessVerifier {
	team := strings.TrimSuffix(strings.TrimPrefix(cfg.TeamDomain, "https://"), "/")
	return &AccessVerifier{
		cfg:     cfg,
		certURL: "https://" + team + "/cdn-cgi/access/certs",
		issuer:  "https://" + team,
		client:  &http.Client{Timeout: 10 * time.Second},
	}
}

type Identity struct {
	Email string `json:"email"`
}

type claims struct {
	Aud   audience `json:"aud"`
	Iss   string   `json:"iss"`
	Exp   int64    `json:"exp"`
	Nbf   int64    `json:"nbf"`
	Email string   `json:"email"`
	Sub   string   `json:"sub"`
}

// audience accepts either a string or an array of strings.
type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*a = []string{s}
		return nil
	}
	var ss []string
	if err := json.Unmarshal(b, &ss); err != nil {
		return err
	}
	*a = ss
	return nil
}

func (v *AccessVerifier) Verify(ctx context.Context, token string) (Identity, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Identity{}, errors.New("malformed jwt")
	}
	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodeSeg(parts[0], &hdr); err != nil {
		return Identity{}, err
	}
	if hdr.Alg != "RS256" {
		return Identity{}, fmt.Errorf("unexpected alg %q", hdr.Alg)
	}
	key, err := v.key(ctx, hdr.Kid)
	if err != nil {
		return Identity{}, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Identity{}, err
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], sig); err != nil {
		return Identity{}, errors.New("bad signature")
	}
	var c claims
	if err := decodeSeg(parts[1], &c); err != nil {
		return Identity{}, err
	}
	now := time.Now().Unix()
	switch {
	case !slices.Contains(c.Aud, v.cfg.AUD):
		return Identity{}, errors.New("wrong audience")
	case c.Iss != v.issuer:
		return Identity{}, errors.New("wrong issuer")
	case c.Exp != 0 && now > c.Exp+60:
		return Identity{}, errors.New("expired")
	case c.Nbf != 0 && now+60 < c.Nbf:
		return Identity{}, errors.New("not yet valid")
	}
	id := Identity{Email: c.Email}
	if id.Email == "" {
		id.Email = "service:" + c.Sub
	}
	return id, nil
}

func decodeSeg(seg string, out any) error {
	b, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

func (v *AccessVerifier) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if k, ok := v.keys[kid]; ok && time.Since(v.fetched) < 24*time.Hour {
		return k, nil
	}
	// Unknown kid or stale cache: refetch, but not more than once a minute.
	if time.Since(v.fetched) > time.Minute || v.keys == nil {
		if err := v.fetch(ctx); err != nil {
			return nil, err
		}
	}
	if k, ok := v.keys[kid]; ok {
		return k, nil
	}
	return nil, errors.New("unknown signing key")
}

func (v *AccessVerifier) fetch(ctx context.Context) error {
	req, _ := http.NewRequestWithContext(ctx, "GET", v.certURL, nil)
	res, err := v.client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch access certs: %w", err)
	}
	defer res.Body.Close()
	var jwks struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(res.Body).Decode(&jwks); err != nil {
		return fmt.Errorf("decode access certs: %w", err)
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range jwks.Keys {
		if k.Kty != "RSA" {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	if len(keys) == 0 {
		return errors.New("no RSA keys in access certs")
	}
	v.keys, v.fetched = keys, time.Now()
	return nil
}
