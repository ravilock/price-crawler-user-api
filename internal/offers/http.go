package offers

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"price-crawler/services/user-api/internal/auth"
	"price-crawler/services/user-api/internal/messaging"
)

const resultQueue = "collection.results.read.requested.v1"

type publisher interface {
	Publish(context.Context, string, messaging.Envelope) error
	RequestReply(context.Context, string, messaging.Envelope, time.Duration) (messaging.Envelope, error)
	Close() error
}

type connect func(string) (publisher, error)

func NewHandler(brokerURL string) http.Handler {
	return handler(brokerURL, func(url string) (publisher, error) { return messaging.NewPublisher(url) })
}

func handler(brokerURL string, open connect) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/offers", func(w http.ResponseWriter, r *http.Request) { submit(w, r, brokerURL, open) })
	mux.HandleFunc("GET /api/v1/offers/results", func(w http.ResponseWriter, r *http.Request) { read(w, r, brokerURL, open) })
	return mux
}

func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

func principal(w http.ResponseWriter, r *http.Request, roles ...string) (auth.Principal, bool) {
	p, ok := auth.PrincipalFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return auth.Principal{}, false
	}
	if !p.Authorize(roles...) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return auth.Principal{}, false
	}
	return p, true
}

func submit(w http.ResponseWriter, r *http.Request, brokerURL string, open connect) {
	p, ok := principal(w, r, "administrator", "manager")
	if !ok {
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 200 {
		http.Error(w, "Idempotency-Key must contain 1 to 200 characters", http.StatusBadRequest)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var body struct {
		URL string `json:"url"`
	}
	if err := decoder.Decode(&body); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	u, err := url.ParseRequestURI(body.URL)
	if err != nil || len(body.URL) > 4096 || u == nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		http.Error(w, "url must be an absolute http or https URL", http.StatusUnprocessableEntity)
		return
	}
	requestID, err := newID()
	if err != nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	correlation, err := newID()
	if err != nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	payload, _ := json.Marshal(map[string]any{"crawl_request_id": requestID, "url": body.URL, "reason": "manual", "idempotency_key": key, "requester_id": p.Subject})
	e := messaging.Envelope{MessageID: requestID, MessageType: "crawl.requested", SchemaVersion: 1, OccurredAt: time.Now().UTC(), Organization: p.Organization, CorrelationID: correlation, IdempotencyKey: key, Payload: payload}
	client, err := open(brokerURL)
	if err != nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	defer func() { _ = client.Close() }()
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := client.Publish(ctx, "crawl.requested.v1", e); err != nil {
		writeBrokerError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{"request_id": requestID, "status": "accepted"})
}

func read(w http.ResponseWriter, r *http.Request, brokerURL string, open connect) {
	p, ok := principal(w, r, "administrator", "manager", "end_user", "auditor")
	if !ok {
		return
	}
	requestID := r.URL.Query().Get("request_id")
	cursor := r.URL.Query().Get("cursor")
	if requestID != "" && cursor != "" {
		http.Error(w, "request_id and cursor cannot be combined", http.StatusBadRequest)
		return
	}
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 100 {
			http.Error(w, "limit must be 1 to 100", http.StatusBadRequest)
			return
		}
		limit = n
	}
	role := "end_user"
	for _, candidate := range []string{"administrator", "manager", "auditor"} {
		if p.HasRole(candidate) {
			role = candidate
			break
		}
	}
	requestIDMessage, err := newID()
	if err != nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	correlation, err := newID()
	if err != nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	requestPayload := map[string]any{"actor": map[string]string{"subject": p.Subject, "role": role}, "cursor": nullable(cursor), "limit": limit}
	if requestID != "" {
		requestPayload["request_id"] = requestID
	}
	payload, _ := json.Marshal(requestPayload)
	e := messaging.Envelope{MessageID: requestIDMessage, MessageType: "collection.results.read.requested", SchemaVersion: 1, OccurredAt: time.Now().UTC(), Organization: p.Organization, CorrelationID: correlation, Payload: payload}
	client, err := open(brokerURL)
	if err != nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	defer func() { _ = client.Close() }()
	reply, err := client.RequestReply(r.Context(), resultQueue, e, 5*time.Second)
	if err != nil {
		writeBrokerError(w, err)
		return
	}
	if reply.MessageType != "collection.results.read.result" || reply.Organization != p.Organization || reply.CorrelationID != correlation || reply.CausationID != requestIDMessage {
		http.Error(w, "invalid broker response", http.StatusBadGateway)
		return
	}
	var result struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(reply.Payload, &result); err != nil {
		http.Error(w, "invalid broker response", http.StatusBadGateway)
		return
	}
	switch result.Status {
	case "ok":
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(json.RawMessage(reply.Payload))
	case "not_found":
		http.Error(w, "not found", http.StatusNotFound)
	case "forbidden":
		http.Error(w, "forbidden", http.StatusForbidden)
	case "invalid":
		http.Error(w, "invalid request", http.StatusUnprocessableEntity)
	case "expired":
		http.Error(w, "cursor expired", http.StatusGone)
	case "not_ready":
		http.Error(w, "results temporarily unavailable", http.StatusServiceUnavailable)
	default:
		http.Error(w, "invalid broker response", http.StatusBadGateway)
	}
}

func nullable(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func writeBrokerError(w http.ResponseWriter, err error) {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, messaging.ErrRequestTimeout) {
		http.Error(w, "upstream timeout", http.StatusGatewayTimeout)
		return
	}
	if errors.Is(err, messaging.ErrUnroutable) {
		http.Error(w, "workflow unavailable", http.StatusServiceUnavailable)
		return
	}
	http.Error(w, "workflow unavailable", http.StatusServiceUnavailable)
}
