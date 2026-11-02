package offers

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"price-crawler/services/user-api/internal/auth"
	"price-crawler/services/user-api/internal/messaging"
)

type fakePublisher struct {
	sent  messaging.Envelope
	queue string
	reply messaging.Envelope
	err   error
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func (f *fakePublisher) Publish(_ context.Context, q string, e messaging.Envelope) error {
	f.queue, f.sent = q, e
	return f.err
}
func (f *fakePublisher) RequestReply(_ context.Context, q string, e messaging.Envelope, _ time.Duration) (messaging.Envelope, error) {
	f.queue, f.sent = q, e
	return f.reply, f.err
}
func (f *fakePublisher) Close() error { return nil }

func authTest(t *testing.T, h http.Handler, role string) (http.Handler, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	issuer := "https://identity.test/realm"
	keys, _ := json.Marshal(map[string]any{"keys": []any{map[string]string{"kid": "test", "kty": "RSA", "alg": "RS256", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())}}})
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "test"})
	claims, _ := json.Marshal(map[string]any{"iss": issuer, "sub": "00000000-0000-4000-8000-000000000011", "organization_id": "00000000-0000-4000-8000-000000000022", "azp": "price-intelligence-spa", "exp": time.Now().Add(time.Hour).Unix(), "realm_access": map[string]any{"roles": []string{role}}})
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	token := unsigned + "." + base64.RawURLEncoding.EncodeToString(signature)
	verifier := auth.NewVerifier(issuer, "https://identity.test/keys")
	verifier.Client = &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(keys))), Header: make(http.Header)}, nil
	})}
	return auth.Require(verifier, role)(h), token
}

func requestAs(t *testing.T, h http.Handler, req *http.Request, role string) *httptest.ResponseRecorder {
	secured, token := authTest(t, h, role)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	secured.ServeHTTP(w, req)
	return w
}

func TestOfferSubmissionRoleAndPayload(t *testing.T) {
	f := &fakePublisher{}
	h := handler("broker", func(string) (publisher, error) { return f, nil })
	denied := httptest.NewRequest(http.MethodPost, "/api/v1/offers", strings.NewReader(`{"url":"https://shop.example/item"}`))
	denied.Header.Set("Idempotency-Key", "k")
	w := requestAs(t, h, denied, "end_user")
	if w.Code != http.StatusForbidden {
		t.Fatalf("end-user submission status=%d", w.Code)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/offers", strings.NewReader(`{"url":"https://shop.example/item"}`))
	r.Header.Set("Idempotency-Key", "k")
	w = requestAs(t, h, r, "manager")
	if w.Code != http.StatusAccepted || f.queue != "crawl.requested.v1" {
		t.Fatalf("manager submission status=%d queue=%s body=%s", w.Code, f.queue, w.Body)
	}
	var payload map[string]any
	if err := json.Unmarshal(f.sent.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["requester_id"] != "00000000-0000-4000-8000-000000000011" || payload["reason"] != "manual" || payload["idempotency_key"] != "k" || f.sent.Organization != "00000000-0000-4000-8000-000000000022" {
		t.Fatalf("untrusted or missing actor context: %#v %#v", payload, f.sent)
	}
}

func TestOfferResultReadMapsScopeAndTransportFailures(t *testing.T) {
	f := &fakePublisher{}
	h := handler("broker", func(string) (publisher, error) { return f, nil })
	request := httptest.NewRequest(http.MethodGet, "/api/v1/offers/results?cursor=abc&limit=10", nil)
	w := requestAs(t, h, request, "end_user")
	var payload map[string]any
	_ = json.Unmarshal(f.sent.Payload, &payload)
	actor := payload["actor"].(map[string]any)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("invalid reply status=%d", w.Code)
	}
	if f.queue != resultQueue || actor["subject"] != "00000000-0000-4000-8000-000000000011" || actor["role"] != "end_user" {
		t.Fatalf("bad result request: queue=%s payload=%v", f.queue, payload)
	}
	f.err = context.DeadlineExceeded
	w = requestAs(t, h, httptest.NewRequest(http.MethodGet, "/api/v1/offers/results", nil), "auditor")
	if w.Code != http.StatusGatewayTimeout {
		t.Fatalf("timeout status=%d", w.Code)
	}
	f.err = errors.New("offline")
	w = requestAs(t, h, httptest.NewRequest(http.MethodGet, "/api/v1/offers/results", nil), "manager")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("offline status=%d", w.Code)
	}
}

func TestOfferSubmissionRejectsMissingIdempotencyAndInvalidURL(t *testing.T) {
	f := &fakePublisher{}
	h := handler("broker", func(string) (publisher, error) { return f, nil })
	for _, tc := range []struct {
		body, key string
		want      int
	}{{`{"url":"https://shop.example/x"}`, "", http.StatusBadRequest}, {`{"url":"file:///etc/passwd"}`, "k", http.StatusUnprocessableEntity}} {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/offers", strings.NewReader(tc.body))
		r.Header.Set("Idempotency-Key", tc.key)
		w := requestAs(t, h, r, "manager")
		if w.Code != tc.want {
			t.Fatalf("status=%d want=%d body=%s", w.Code, tc.want, w.Body)
		}
	}
}
