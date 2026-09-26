package router_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/blueship581/hazardous-waste-transfer-compliance/backend/internal/config"
	"github.com/blueship581/hazardous-waste-transfer-compliance/backend/internal/database"
	"github.com/blueship581/hazardous-waste-transfer-compliance/backend/internal/router"
	"github.com/gin-gonic/gin"
)

type apiEnvelope struct {
	Data  json.RawMessage `json:"data"`
	Error string          `json:"error"`
	Meta  struct {
		Total int64 `json:"total"`
	} `json:"meta"`
}

type record struct {
	ID      uint   `json:"id"`
	Code    string `json:"code"`
	Status  string `json:"status"`
	Version uint   `json:"version"`
}

func TestRBACLinkedComplianceWorkflowAndAuditing(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := testConfig(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, redisClient, err := database.Open(context.Background(), cfg, logger)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	if redisClient != nil {
		t.Fatal("test must use in-memory limiter without Redis")
	}
	engine := router.New(cfg, db, nil, logger)

	viewer := login(t, engine, "viewer")
	operator := login(t, engine, "operator")
	reviewer := login(t, engine, "reviewer")

	response, _ := request(t, engine, http.MethodGet, "/api/generators?page=1&pageSize=10", viewer, "", nil)
	assertStatus(t, response, http.StatusOK)
	response, _ = request(t, engine, http.MethodPost, "/api/manifests", viewer, "viewer-write", manifestPayload("TM-VIEWER", "CP-002"))
	assertStatus(t, response, http.StatusForbidden)
	response, _ = request(t, engine, http.MethodGet, "/api/audits", viewer, "", nil)
	assertStatus(t, response, http.StatusForbidden)

	response, body := request(t, engine, http.MethodPost, "/api/manifests", operator, "manifest-create-valid", manifestPayload("TM-ROUTER-001", "CP-002"))
	assertStatus(t, response, http.StatusCreated)
	manifest := decodeRecord(t, body)
	if manifest.Status != "draft" || response.Header.Get("X-Request-ID") != "manifest-create-valid" {
		t.Fatalf("unexpected manifest response: %+v request-id=%q", manifest, response.Header.Get("X-Request-ID"))
	}

	response, _ = request(t, engine, http.MethodPost, fmt.Sprintf("/api/manifests/%d/transition", manifest.ID), operator, "manifest-skip", map[string]any{
		"status": "in_transit", "expectedVersion": manifest.Version, "reason": "must not skip submission",
	})
	assertStatus(t, response, http.StatusUnprocessableEntity)

	response, body = request(t, engine, http.MethodPost, fmt.Sprintf("/api/manifests/%d/transition", manifest.ID), operator, "manifest-submit", map[string]any{
		"status": "submitted", "expectedVersion": manifest.Version, "reason": "linked permits checked",
	})
	assertStatus(t, response, http.StatusOK)
	manifest = decodeRecord(t, body)
	if manifest.Status != "submitted" || manifest.Version != 2 {
		t.Fatalf("manifest transition was not persisted: %+v", manifest)
	}

	response, _ = request(t, engine, http.MethodPost, fmt.Sprintf("/api/manifests/%d/transition", manifest.ID), operator, "manifest-stale", map[string]any{
		"status": "in_transit", "expectedVersion": uint(1), "reason": "stale client must conflict",
	})
	assertStatus(t, response, http.StatusConflict)

	response, body = request(t, engine, http.MethodPost, "/api/manifests", operator, "manifest-create-unverified", manifestPayload("TM-ROUTER-002", "CP-001"))
	assertStatus(t, response, http.StatusCreated)
	unverified := decodeRecord(t, body)
	response, _ = request(t, engine, http.MethodPost, fmt.Sprintf("/api/manifests/%d/transition", unverified.ID), operator, "manifest-block-unverified", map[string]any{
		"status": "submitted", "expectedVersion": unverified.Version, "reason": "must verify carrier first",
	})
	assertStatus(t, response, http.StatusUnprocessableEntity)

	response, body = request(t, engine, http.MethodPost, "/api/manifests", operator, "manifest-create-rejected", manifestPayload("TM-ROUTER-003", "CP-002"))
	assertStatus(t, response, http.StatusCreated)
	rejected := decodeRecord(t, body)
	response, body = request(t, engine, http.MethodPost, fmt.Sprintf("/api/manifests/%d/transition", rejected.ID), operator, "manifest-submit-rejected", map[string]any{
		"status": "submitted", "expectedVersion": rejected.Version, "reason": "linked permits checked",
	})
	assertStatus(t, response, http.StatusOK)
	rejected = decodeRecord(t, body)
	response, _ = request(t, engine, http.MethodPost, fmt.Sprintf("/api/manifests/%d/transition", rejected.ID), operator, "manifest-reject", map[string]any{
		"status": "rejected", "expectedVersion": rejected.Version, "reason": "destination permit mismatch",
	})
	assertStatus(t, response, http.StatusOK)
	response, body = request(t, engine, http.MethodPost, "/api/checks", operator, "check-create-rejected", checkPayload("CC-ROUTER-002", rejected.Code))
	assertStatus(t, response, http.StatusCreated)
	rejectedCheck := decodeRecord(t, body)
	response, _ = request(t, engine, http.MethodPost, fmt.Sprintf("/api/checks/%d/transition", rejectedCheck.ID), reviewer, "rejected-manifest-pass", map[string]any{
		"status": "pass", "expectedVersion": rejectedCheck.Version, "reason": "a rejected manifest must not pass",
	})
	assertStatus(t, response, http.StatusUnprocessableEntity)

	response, body = request(t, engine, http.MethodPost, "/api/checks", operator, "check-create", checkPayload("CC-ROUTER-001", manifest.Code))
	assertStatus(t, response, http.StatusCreated)
	check := decodeRecord(t, body)
	response, _ = request(t, engine, http.MethodPost, fmt.Sprintf("/api/checks/%d/transition", check.ID), operator, "operator-decision", map[string]any{
		"status": "pass", "expectedVersion": check.Version, "reason": "operator must not decide",
	})
	assertStatus(t, response, http.StatusForbidden)

	response, body = request(t, engine, http.MethodPost, fmt.Sprintf("/api/checks/%d/transition", check.ID), reviewer, "reviewer-decision", map[string]any{
		"status": "pass", "expectedVersion": check.Version, "reason": "all four evidence groups verified",
	})
	assertStatus(t, response, http.StatusOK)
	check = decodeRecord(t, body)
	if check.Status != "pass" || check.Version != 2 {
		t.Fatalf("review decision was not persisted: %+v", check)
	}

	response, body = request(t, engine, http.MethodGet, "/api/audits?page=1&pageSize=100", reviewer, "audit-read", nil)
	assertStatus(t, response, http.StatusOK)
	if !bytes.Contains(body, []byte("manifest-submit")) || !bytes.Contains(body, []byte("reviewer-decision")) {
		t.Fatalf("expected request IDs in immutable audit list: %s", string(body))
	}
	var envelope apiEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Meta.Total < 5 {
		t.Fatalf("expected audited mutations, got total=%d error=%v", envelope.Meta.Total, err)
	}
}

func TestShipmentGateRequiresMatchingVersionedPassDecision(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := testConfig(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, _, err := database.Open(context.Background(), cfg, logger)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	engine := router.New(cfg, db, nil, logger)

	operator := login(t, engine, "operator")
	reviewer := login(t, engine, "reviewer")

	createSubmittedManifest := func(code string) record {
		response, body := request(t, engine, http.MethodPost, "/api/manifests", operator, code+"-create", manifestPayload(code, "CP-002"))
		assertStatus(t, response, http.StatusCreated)
		manifest := decodeRecord(t, body)
		response, body = request(t, engine, http.MethodPost, fmt.Sprintf("/api/manifests/%d/transition", manifest.ID), operator, code+"-submit", map[string]any{
			"status": "submitted", "expectedVersion": manifest.Version, "reason": "linked permits checked",
		})
		assertStatus(t, response, http.StatusOK)
		return decodeRecord(t, body)
	}
	createCheck := func(code, manifestCode, requestID string) record {
		response, body := request(t, engine, http.MethodPost, "/api/checks", operator, requestID, checkPayload(code, manifestCode))
		assertStatus(t, response, http.StatusCreated)
		return decodeRecord(t, body)
	}
	decide := func(check record, status, requestID string) record {
		response, body := request(t, engine, http.MethodPost, fmt.Sprintf("/api/checks/%d/transition", check.ID), reviewer, requestID, map[string]any{
			"status": status, "expectedVersion": check.Version, "reason": "gate test decision " + status,
		})
		if response.StatusCode != http.StatusOK {
			t.Fatalf("decide %s %s v%d failed: HTTP %d body=%s", check.Code, status, check.Version, response.StatusCode, string(body))
		}
		return decodeRecord(t, body)
	}
	ship := func(manifest record, requestID string) (*http.Response, []byte) {
		return request(t, engine, http.MethodPost, fmt.Sprintf("/api/manifests/%d/transition", manifest.ID), operator, requestID, map[string]any{
			"status": "in_transit", "expectedVersion": manifest.Version, "reason": "load ready",
		})
	}
	blockMessage := func(response *http.Response, body []byte, requestID string) string {
		t.Helper()
		assertStatus(t, response, http.StatusUnprocessableEntity)
		var envelope struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(body, &envelope); err != nil {
			t.Fatalf("decode block response for %s: %v", requestID, err)
		}
		return envelope.Message
	}

	// 1. No compliance check exists for the manifest.
	manifest := createSubmittedManifest("TM-GATE-NONE")
	response, body := ship(manifest, "gate-block-no-record")
	message := blockMessage(response, body, "gate-block-no-record")
	if !strings.Contains(message, "暂无任何合规核验记录") {
		t.Fatalf("missing-record block must explain the gap, got %q", message)
	}

	// 2. Check exists but the reviewer has not decided yet.
	manifest = createSubmittedManifest("TM-GATE-PENDING")
	check := createCheck("CC-GATE-PENDING", manifest.Code, "gate-check-pending")
	response, body = ship(manifest, "gate-block-pending")
	message = blockMessage(response, body, "gate-block-pending")
	if !strings.Contains(message, check.Code) || !strings.Contains(message, "仍待处理") {
		t.Fatalf("pending block must name check %s and pending reason, got %q", check.Code, message)
	}

	// 3. Reviewer rejected the load.
	manifest = createSubmittedManifest("TM-GATE-FAIL")
	check = createCheck("CC-GATE-FAIL", manifest.Code, "gate-check-fail")
	check = decide(check, "fail", "gate-decide-fail")
	response, body = ship(manifest, "gate-block-fail")
	message = blockMessage(response, body, "gate-block-fail")
	if !strings.Contains(message, check.Code) || !strings.Contains(message, "不通过") {
		t.Fatalf("fail block must name check %s and fail reason, got %q", check.Code, message)
	}

	// 4. The rejected check was escalated; escalation is still not a release.
	check = decide(check, "escalated", "gate-decide-escalated")
	response, body = ship(manifest, "gate-block-escalated")
	message = blockMessage(response, body, "gate-block-escalated")
	if !strings.Contains(message, check.Code) || !strings.Contains(message, "升级复核") {
		t.Fatalf("escalated block must name check %s and escalation reason, got %q", check.Code, message)
	}

	// 5. Check passed against an older manifest version; the load changed meanwhile.
	response, body = request(t, engine, http.MethodPost, "/api/manifests", operator, "gate-stale-create", manifestPayload("TM-GATE-STALE", "CP-002"))
	assertStatus(t, response, http.StatusCreated)
	staleManifest := decodeRecord(t, body)
	staleCheck := createCheck("CC-GATE-STALE", staleManifest.Code, "gate-check-stale-v1")
	response, _ = request(t, engine, http.MethodPost, fmt.Sprintf("/api/manifests/%d/transition", staleManifest.ID), operator, "gate-stale-submit", map[string]any{
		"status": "submitted", "expectedVersion": staleManifest.Version, "reason": "now submitted at v2",
	})
	assertStatus(t, response, http.StatusOK)
	staleCheck = decide(staleCheck, "pass", "gate-decide-stale-pass")
	response, body = request(t, engine, http.MethodGet, fmt.Sprintf("/api/checks/%d", staleCheck.ID), reviewer, "", nil)
	assertStatus(t, response, http.StatusOK)
	var checkEnvelope struct {
		Data struct {
			ManifestVersion uint `json:"manifestVersion"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &checkEnvelope); err != nil || checkEnvelope.Data.ManifestVersion != 1 {
		t.Fatalf("check must snapshot manifest v1, got %+v err=%v", checkEnvelope.Data, err)
	}
	response, body = request(t, engine, http.MethodPost, fmt.Sprintf("/api/manifests/%d/transition", staleManifest.ID), operator, "gate-block-version", map[string]any{
		"status": "in_transit", "expectedVersion": staleManifest.Version + 1, "reason": "stale decision must not release",
	})
	message = blockMessage(response, body, "gate-block-version")
	if !strings.Contains(message, staleCheck.Code) || !strings.Contains(message, "v1") || !strings.Contains(message, "v2") {
		t.Fatalf("version block must name check %s and both versions, got %q", staleCheck.Code, message)
	}

	// 6. Matching code + version with a pass decision releases the load.
	manifest = createSubmittedManifest("TM-GATE-PASS")
	check = createCheck("CC-GATE-PASS", manifest.Code, "gate-check-pass")
	decide(check, "pass", "gate-decide-pass")
	response, body = ship(manifest, "gate-release-pass")
	assertStatus(t, response, http.StatusOK)
	shipped := decodeRecord(t, body)
	if shipped.Status != "in_transit" || shipped.Version != 3 {
		t.Fatalf("matching pass should release shipment, got %+v", shipped)
	}

	// 7. Every block and the successful release are audited under their request IDs.
	response, body = request(t, engine, http.MethodGet, "/api/audits?page=1&pageSize=100", reviewer, "gate-audit-read", nil)
	assertStatus(t, response, http.StatusOK)
	for _, expected := range []string{"gate-block-no-record", "gate-block-pending", "gate-block-fail", "gate-block-escalated", "gate-block-version", "gate-release-pass", "shipment_blocked", "CC-GATE-PASS"} {
		if !bytes.Contains(body, []byte(expected)) {
			t.Fatalf("audit trail must contain %q", expected)
		}
	}
}

func testConfig(t *testing.T) config.Config {
	t.Helper()
	return config.Config{
		AppName: "hazardous-waste-transfer-compliance-test", Environment: "test", Port: "0",
		DatabaseDriver: "sqlite", DatabaseDSN: "file:router-test?mode=memory&cache=shared",
		JWTSecret: "router-test-secret-at-least-32-characters", TokenTTL: time.Hour,
		RequestLimit: 10000, StartupTimeout: 5 * time.Second, ShutdownTimeout: 5 * time.Second,
		ReadHeaderTimeout: time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 10 * time.Second,
	}
}

func login(t *testing.T, engine http.Handler, username string) string {
	t.Helper()
	response, body := request(t, engine, http.MethodPost, "/api/auth/login", "", "", map[string]any{
		"username": username, "password": "Admin123!",
	})
	assertStatus(t, response, http.StatusOK)
	var envelope struct {
		Data struct {
			Token string `json:"token"`
			Role  string `json:"role"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Data.Token == "" || envelope.Data.Role != username {
		t.Fatalf("login %s failed: role=%q error=%v body=%s", username, envelope.Data.Role, err, string(body))
	}
	return envelope.Data.Token
}

func request(t *testing.T, engine http.Handler, method, path, token, requestID string, payload any) (*http.Response, []byte) {
	t.Helper()
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("encode request: %v", err)
		}
		body = bytes.NewReader(encoded)
	}
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, body)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if requestID != "" {
		req.Header.Set("X-Request-ID", requestID)
	}
	engine.ServeHTTP(recorder, req)
	return recorder.Result(), recorder.Body.Bytes()
}

func assertStatus(t *testing.T, response *http.Response, expected int) {
	t.Helper()
	if response.StatusCode != expected {
		t.Fatalf("expected HTTP %d, got %d", expected, response.StatusCode)
	}
}

func decodeRecord(t *testing.T, body []byte) record {
	t.Helper()
	var envelope struct {
		Data record `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Data.ID == 0 {
		t.Fatalf("decode record: %v body=%s", err, string(body))
	}
	return envelope.Data
}

func manifestPayload(code, carrier string) map[string]any {
	return map[string]any{
		"code": code, "name": "路由集成测试联单", "description": "valid linked transfer manifest",
		"generatorCode": "WG-001", "carrierCode": carrier, "wasteCode": "HW08-900-249-08", "quantityKg": 680.5,
		"destination": "合规处置中心 A", "facility": "东区危废暂存区", "owner": "operator", "category": "危废转运",
		"riskLevel": "medium", "metricValue": 68, "metricUnit": "score", "effectiveAt": time.Now().UTC().Format(time.RFC3339),
		"evidence": "minio://evidence/tests/manifest.pdf", "relatedCode": strings.ReplaceAll(code, "TM", "REL"),
	}
}

func checkPayload(code, manifest string) map[string]any {
	return map[string]any{
		"code": code, "name": "路由集成测试核验", "description": "linked compliance decision",
		"manifestCode": manifest, "checklist": "产废许可、承运资质、联单数量、处置去向", "decisionBasis": "",
		"facility": "复核中心", "owner": "reviewer", "category": "联单复核", "riskLevel": "medium",
		"metricValue": 92, "metricUnit": "score", "effectiveAt": time.Now().UTC().Format(time.RFC3339),
		"evidence": "minio://evidence/tests/check.pdf", "relatedCode": manifest,
	}
}
