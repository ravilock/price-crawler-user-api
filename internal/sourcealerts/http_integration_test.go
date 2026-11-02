package sourcealerts

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
	"os"
	"strings"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"price-crawler/services/user-api/internal/auth"
	"price-crawler/services/user-api/internal/messaging"
)

func TestAdministratorNotificationHTTP(t *testing.T) {
	broker := os.Getenv("USERAPI_TEST_RABBITMQ_URL")
	if broker == "" {
		t.Skip("set USERAPI_TEST_RABBITMQ_URL to a disposable broker")
	}
	var conn *amqp.Connection
	var err error
	for i := 0; i < 100; i++ {
		conn, err = amqp.Dial(broker)
		if err == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	channel, err := conn.Channel()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = channel.Close() }()
	queue := "source.alerts.read.requested.v1"
	if _, err = channel.QueueDeclare(queue, true, false, false, false, nil); err != nil {
		t.Fatal(err)
	}
	deliveries, err := channel.Consume(queue, "", true, false, false, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	responderDone := make(chan struct{})
	go func() {
		defer close(responderDone)
		select {
		case message := <-deliveries:
			request, err := messaging.Decode(message.Body)
			if err != nil {
				return
			}
			payload, _ := json.Marshal(map[string]any{"status": "ok", "items": []map[string]any{{"id": "00000000-0000-4000-8000-000000000002", "source_id": "00000000-0000-4000-8000-000000000003", "domain": "store.example", "occurred_at": time.Now().UTC()}}})
			reply := messaging.Envelope{MessageID: "00000000-0000-4000-8000-000000000004", MessageType: "source.alerts.read.result", SchemaVersion: 1, OccurredAt: time.Now(), Organization: request.Organization, CorrelationID: request.CorrelationID, CausationID: request.MessageID, Payload: payload}
			raw, _ := reply.Encode()
			_ = channel.PublishWithContext(context.Background(), "", message.ReplyTo, false, false, amqp.Publishing{CorrelationId: request.CorrelationID, Body: raw})
		case <-time.After(10 * time.Second):
		}
	}()
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]string{"kid": "test", "kty": "RSA", "alg": "RS256", "use": "sig", "n": base64.RawURLEncoding.EncodeToString(private.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(private.E)).Bytes())}}})
	}))
	defer jwks.Close()
	token := func(role string) string {
		header, _ := json.Marshal(map[string]any{"alg": "RS256", "kid": "test"})
		claims, _ := json.Marshal(map[string]any{"iss": jwks.URL, "sub": "admin", "organization_id": "00000000-0000-4000-8000-000000000001", "azp": "price-intelligence-spa", "exp": time.Now().Add(time.Hour).Unix(), "realm_access": map[string]any{"roles": []string{role}}})
		unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
		hash := sha256.Sum256([]byte(unsigned))
		signature, err := rsa.SignPKCS1v15(rand.Reader, private, crypto.SHA256, hash[:])
		if err != nil {
			t.Fatal(err)
		}
		return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature)
	}
	handler := auth.Require(auth.NewVerifier(jwks.URL, jwks.URL), "administrator")(Handler(broker))
	call := func(role string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/admin/source-alerts", nil)
		r.Header.Set("Authorization", "Bearer "+token(role))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if denied := call("end_user"); denied.Code != http.StatusForbidden {
		t.Fatalf("end-user status=%d", denied.Code)
	}
	response := call("administrator")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "store.example") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	<-responderDone
}
