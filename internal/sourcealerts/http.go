package sourcealerts

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"price-crawler/services/user-api/internal/auth"
	"price-crawler/services/user-api/internal/messaging"
)

func id() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}
func Handler(brokerURL string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, ok := auth.PrincipalFromContext(r.Context())
		if !ok || !principal.HasRole("administrator") {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		requestID, err := id()
		if err != nil {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		correlation, err := id()
		if err != nil {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		payload, _ := json.Marshal(map[string]any{"actor": map[string]string{"subject": principal.Subject, "role": "administrator"}, "limit": 50})
		request := messaging.Envelope{MessageID: requestID, MessageType: "source.alerts.read.requested", SchemaVersion: 1, OccurredAt: time.Now().UTC(), Organization: principal.Organization, CorrelationID: correlation, Payload: payload}
		publisher, err := messaging.NewPublisher(brokerURL)
		if err != nil {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		defer func() { _ = publisher.Close() }()
		reply, err := publisher.RequestReply(r.Context(), "source.alerts.read.requested.v1", request, 5*time.Second)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				http.Error(w, "timeout", http.StatusGatewayTimeout)
			} else {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
			}
			return
		}
		if reply.MessageType != "source.alerts.read.result" || reply.Organization != principal.Organization || reply.CorrelationID != correlation || reply.CausationID != requestID {
			http.Error(w, "invalid response", http.StatusBadGateway)
			return
		}
		var body struct {
			Status string `json:"status"`
			Items  []struct {
				ID         string    `json:"id"`
				SourceID   string    `json:"source_id"`
				Domain     string    `json:"domain"`
				OccurredAt time.Time `json:"occurred_at"`
			} `json:"items"`
		}
		if json.Unmarshal(reply.Payload, &body) != nil {
			http.Error(w, "invalid response", http.StatusBadGateway)
			return
		}
		if body.Status != "ok" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	})
}
