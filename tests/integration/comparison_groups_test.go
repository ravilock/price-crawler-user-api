package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

type response struct {
	status int
	body   []byte
}

func request(t *testing.T, base, token, method, path, key string, body any) response {
	t.Helper()
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		payload = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, strings.TrimRight(base, "/")+path, payload)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	return response{status: res.StatusCode, body: raw}
}

func decodeObject(t *testing.T, res response) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(res.body, &value); err != nil {
		t.Fatalf("response is not JSON: status=%d body=%s: %v", res.status, res.body, err)
	}
	return value
}

func requireStatus(t *testing.T, res response, want int) {
	t.Helper()
	if res.status != want {
		t.Fatalf("status=%d want=%d body=%s", res.status, want, res.body)
	}
}

func TestComparisonManagerWorkflowBoundaries(t *testing.T) {
	base := os.Getenv("USERAPI_TEST_BASE_URL")
	manager := os.Getenv("USERAPI_TEST_MANAGER_TOKEN")
	endUser := os.Getenv("USERAPI_TEST_END_USER_TOKEN")
	foreignManager := os.Getenv("USERAPI_TEST_FOREIGN_MANAGER_TOKEN")
	if base == "" || manager == "" || endUser == "" || foreignManager == "" {
		t.Skip("set USERAPI_TEST_BASE_URL and manager, end-user, and foreign-manager bearer tokens for an isolated integrated environment")
	}

	search := request(t, base, manager, http.MethodGet, "/api/v1/catalog/products?limit=100", "", nil)
	requireStatus(t, search, http.StatusOK)
	items, ok := decodeObject(t, search)["items"].([]any)
	if !ok || len(items) < 2 {
		t.Fatalf("need two tenant-owned canonical products from catalog search, got %s", search.body)
	}
	productIDs := make([]string, 2)
	for i := range productIDs {
		item, ok := items[i].(map[string]any)
		if !ok {
			t.Fatalf("catalog item %d is not an object", i)
		}
		productIDs[i], ok = item["id"].(string)
		if !ok || productIDs[i] == "" {
			t.Fatalf("catalog item %d has no id", i)
		}
	}
	historyBefore := request(t, base, manager, http.MethodGet, "/api/v1/offers/results?limit=100", "", nil)
	requireStatus(t, historyBefore, http.StatusOK)

	foreignSearch := request(t, base, foreignManager, http.MethodGet, "/api/v1/catalog/products?limit=100", "", nil)
	requireStatus(t, foreignSearch, http.StatusOK)
	foreignItems, ok := decodeObject(t, foreignSearch)["items"].([]any)
	if !ok || len(foreignItems) == 0 {
		t.Fatalf("need a foreign-tenant canonical product for isolation check, got %s", foreignSearch.body)
	}
	foreignProduct := foreignItems[0].(map[string]any)["id"].(string)

	createBody := map[string]any{"name": "T127 workflow", "product_ids": []string{productIDs[0]}}
	created := request(t, base, manager, http.MethodPost, "/api/v1/comparisons", "t127-create", createBody)
	requireStatus(t, created, http.StatusCreated)
	createdObject := decodeObject(t, created)
	groupID, ok := createdObject["group_id"].(string)
	if !ok || groupID == "" || createdObject["revision"] != float64(1) || createdObject["state"] != "draft" {
		t.Fatalf("invalid draft creation result: %s", created.body)
	}

	// A replay with the same key and body is the original operation result;
	// a different body on that key must conflict.
	replayed := request(t, base, manager, http.MethodPost, "/api/v1/comparisons", "t127-create", createBody)
	requireStatus(t, replayed, http.StatusCreated)
	if !bytes.Equal(created.body, replayed.body) {
		t.Fatalf("same-key replay changed result: first=%s replay=%s", created.body, replayed.body)
	}
	endUserDrafts := request(t, base, endUser, http.MethodGet, "/api/v1/comparisons?limit=100", "", nil)
	requireStatus(t, endUserDrafts, http.StatusOK)
	if strings.Contains(string(endUserDrafts.body), groupID) {
		t.Fatalf("end-user search exposed an unvalidated draft: %s", endUserDrafts.body)
	}
	conflictBody := map[string]any{"name": "different body", "product_ids": productIDs}
	requireStatus(t, request(t, base, manager, http.MethodPost, "/api/v1/comparisons", "t127-create", conflictBody), http.StatusConflict)

	addPath := "/api/v1/comparisons/" + groupID + "/products"
	added := request(t, base, manager, http.MethodPost, addPath, "t127-add", map[string]any{"expected_revision": 1, "product_ids": []string{productIDs[1]}})
	requireStatus(t, added, http.StatusOK)
	addedObject := decodeObject(t, added)
	if addedObject["revision"] != float64(2) {
		t.Fatalf("add revision=%v want=2", addedObject["revision"])
	}
	// Retrying a stale expected revision conflicts instead of overwriting.
	requireStatus(t, request(t, base, manager, http.MethodPost, addPath, "t127-stale", map[string]any{"expected_revision": 1, "product_ids": []string{productIDs[0]}}), http.StatusConflict)

	removePath := fmt.Sprintf("/api/v1/comparisons/%s/products/%s", groupID, productIDs[1])
	removed := request(t, base, manager, http.MethodDelete, removePath, "t127-remove", map[string]any{"expected_revision": 2})
	requireStatus(t, removed, http.StatusOK)
	// Removing the final product is rejected and leaves the group validatable.
	lastPath := fmt.Sprintf("/api/v1/comparisons/%s/products/%s", groupID, productIDs[0])
	requireStatus(t, request(t, base, manager, http.MethodDelete, lastPath, "t127-last-product", map[string]any{"expected_revision": 3}), http.StatusConflict)

	// A caller cannot attach a product belonging to another organization.
	foreignCreate := request(t, base, manager, http.MethodPost, "/api/v1/comparisons", "t127-foreign", map[string]any{"name": "foreign product", "product_ids": []string{foreignProduct}})
	if foreignCreate.status != http.StatusNotFound && foreignCreate.status != http.StatusUnprocessableEntity {
		t.Fatalf("foreign product was not rejected: status=%d body=%s", foreignCreate.status, foreignCreate.body)
	}

	validatePath := "/api/v1/comparisons/" + groupID + "/validate"
	validated := request(t, base, manager, http.MethodPost, validatePath, "t127-validate", map[string]any{"expected_revision": 3})
	requireStatus(t, validated, http.StatusOK)
	if value := decodeObject(t, validated); value["state"] != "validated" || value["revision"] != float64(4) {
		t.Fatalf("validation result=%s", validated.body)
	}
	groupPath := "/api/v1/comparisons/" + groupID
	endUserRead := request(t, base, endUser, http.MethodGet, groupPath, "", nil)
	requireStatus(t, endUserRead, http.StatusOK)
	endUserMutation := request(t, base, endUser, http.MethodPost, addPath, "t127-end-user-edit", map[string]any{"expected_revision": 4, "product_ids": []string{productIDs[0]}})
	requireStatus(t, endUserMutation, http.StatusForbidden)

	// Verify the denied request had no effect and group edits did not alter the
	// canonical products returned by the owner search.
	managerRead := request(t, base, manager, http.MethodGet, groupPath, "", nil)
	requireStatus(t, managerRead, http.StatusOK)
	final := decodeObject(t, managerRead)
	if final["revision"] != float64(4) || final["state"] != "validated" {
		t.Fatalf("forbidden edit changed group state: %s", managerRead.body)
	}
	validatedItems, ok := decodeObject(t, request(t, base, endUser, http.MethodGet, "/api/v1/comparisons?limit=100", "", nil))["items"].([]any)
	if !ok {
		t.Fatal("end-user group search did not return items")
	}
	visible := false
	for _, raw := range validatedItems {
		if item, ok := raw.(map[string]any); ok && item["id"] == groupID {
			visible = true
		}
	}
	if !visible {
		t.Fatalf("validated group %s is absent from end-user search", groupID)
	}
	afterSearch := request(t, base, manager, http.MethodGet, "/api/v1/catalog/products?limit=100", "", nil)
	requireStatus(t, afterSearch, http.StatusOK)
	if !bytes.Equal(search.body, afterSearch.body) {
		t.Fatalf("group workflow unexpectedly changed catalog products: before=%s after=%s", search.body, afterSearch.body)
	}
	historyAfter := request(t, base, manager, http.MethodGet, "/api/v1/offers/results?limit=100", "", nil)
	requireStatus(t, historyAfter, http.StatusOK)
	if !bytes.Equal(historyBefore.body, historyAfter.body) {
		t.Fatalf("group workflow unexpectedly changed collection result history: before=%s after=%s", historyBefore.body, historyAfter.body)
	}
}
