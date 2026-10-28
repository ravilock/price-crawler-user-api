package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestIssuerDiscoveryUsesInternalAddressAndChecksBrowserIssuer(t *testing.T) {
	original := http.DefaultClient
	t.Cleanup(func() { http.DefaultClient = original })
	issuer := "https://browser.example/realms/price-intelligence"
	returned := issuer
	http.DefaultClient = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "http://keycloak:8080/realms/price-intelligence/.well-known/openid-configuration" {
			t.Errorf("unexpected discovery address: %s", r.URL)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"issuer":"` + returned + `"}`)), Header: make(http.Header)}, nil
	})}
	jwks := "http://keycloak:8080/realms/price-intelligence/protocol/openid-connect/certs"
	if err := checkIssuer(context.Background(), issuer, jwks); err != nil {
		t.Fatal(err)
	}
	returned = "https://wrong.example/realms/price-intelligence"
	if err := checkIssuer(context.Background(), issuer, jwks); err == nil {
		t.Fatal("issuer mismatch accepted")
	}
	if err := checkIssuer(context.Background(), issuer, "http://keycloak:8080/wrong"); err == nil {
		t.Fatal("invalid JWKS address accepted")
	}
}
