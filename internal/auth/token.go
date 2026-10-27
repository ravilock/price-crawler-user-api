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
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"
)

type Principal struct {
	Subject, Organization string
	Roles                 []string
}
type jwks struct {
	Keys []struct{ Kid, Kty, Alg, Use, N, E string }
}
type claims struct {
	Issuer          string `json:"iss"`
	Subject         string `json:"sub"`
	Organization    string `json:"organization_id"`
	AuthorizedParty string `json:"azp"`
	Expires         int64  `json:"exp"`
	NotBefore       int64  `json:"nbf"`
	Roles           struct {
		Realm []string `json:"roles"`
	} `json:"realm_access"`
	Resource map[string]struct {
		Roles []string `json:"roles"`
	} `json:"resource_access"`
}

// Verifier validates Keycloak RS256 access tokens against the realm JWKS endpoint.
type Verifier struct {
	Issuer, JWKSURL string
	Client          *http.Client
}

func NewVerifier(issuer, jwksURL string) Verifier {
	issuer = strings.TrimRight(issuer, "/")
	if jwksURL == "" {
		jwksURL = issuer + "/protocol/openid-connect/certs"
	}
	return Verifier{Issuer: issuer, JWKSURL: jwksURL}
}
func (v Verifier) Verify(ctx context.Context, token string) (Principal, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Principal{}, errors.New("malformed bearer token")
	}
	decode := func(s string, dst any) error {
		b, e := base64.RawURLEncoding.DecodeString(s)
		if e != nil {
			return e
		}
		return json.Unmarshal(b, dst)
	}
	var header struct{ Alg, Kid string }
	if err := decode(parts[0], &header); err != nil || header.Alg != "RS256" || header.Kid == "" {
		return Principal{}, errors.New("unsupported token signing header")
	}
	var c claims
	if err := decode(parts[1], &c); err != nil {
		return Principal{}, errors.New("invalid token claims")
	}
	if c.Issuer != v.Issuer || c.Subject == "" || c.Organization == "" || c.AuthorizedParty != "price-intelligence-spa" || c.Expires <= time.Now().Unix() || c.NotBefore > time.Now().Unix() {
		return Principal{}, errors.New("token issuer, client, organization, subject, or validity is invalid")
	}
	client := v.Client
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.JWKSURL, nil)
	if err != nil {
		return Principal{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return Principal{}, fmt.Errorf("fetch signing keys: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Principal{}, errors.New("identity provider signing keys unavailable")
	}
	var keys jwks
	if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&keys); err != nil {
		return Principal{}, errors.New("invalid signing keys response")
	}
	var key *rsa.PublicKey
	for _, k := range keys.Keys {
		if k.Kid == header.Kid && k.Kty == "RSA" && (k.Alg == "" || k.Alg == "RS256") {
			n, e1 := base64.RawURLEncoding.DecodeString(k.N)
			e2, _ := base64.RawURLEncoding.DecodeString(k.E)
			if e1 == nil && len(e2) > 0 {
				exponent := 0
				for _, b := range e2 {
					exponent = exponent<<8 | int(b)
				}
				key = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: exponent}
			}
			break
		}
	}
	if key == nil {
		return Principal{}, errors.New("no matching RSA signing key")
	}
	signed := []byte(parts[0] + "." + parts[1])
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Principal{}, errors.New("invalid token signature")
	}
	sum := sha256.Sum256(signed)
	if err = rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], sig); err != nil {
		return Principal{}, errors.New("invalid token signature")
	}
	roles := append([]string{}, c.Roles.Realm...)
	roles = append(roles, c.Resource["price-intelligence-spa"].Roles...)
	return Principal{Subject: c.Subject, Organization: c.Organization, Roles: roles}, nil
}

func (p Principal) HasRole(role string) bool {
	for _, r := range p.Roles {
		if r == role {
			return true
		}
	}
	return false
}
func (p Principal) Authorize(allowed ...string) bool {
	for _, r := range allowed {
		if p.HasRole(r) {
			return true
		}
	}
	return false
}

type principalContextKey struct{}

func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalContextKey{}).(Principal)
	return p, ok
}

// Require verifies the bearer token before exposing its role and organization claims to handlers.
func Require(verifier Verifier, allowedRoles ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			header := strings.TrimSpace(r.Header.Get("Authorization"))
			if !strings.HasPrefix(header, "Bearer ") {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			principal, err := verifier.Verify(r.Context(), strings.TrimSpace(strings.TrimPrefix(header, "Bearer ")))
			if err != nil {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if len(allowedRoles) > 0 && !principal.Authorize(allowedRoles...) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalContextKey{}, principal)))
		})
	}
}

func RequireOrganization(principal Principal, organizationID string) error {
	if organizationID == "" || principal.Organization != organizationID {
		return errors.New("organization access denied")
	}
	return nil
}
