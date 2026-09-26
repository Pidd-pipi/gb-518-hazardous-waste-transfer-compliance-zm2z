package router_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/blueship581/hazardous-waste-transfer-compliance/backend/internal/database"
	"github.com/blueship581/hazardous-waste-transfer-compliance/backend/internal/router"
	"github.com/gin-gonic/gin"
)

type checkRecord struct {
	ID              uint   `json:"id"`
	Code            string `json:"code"`
	Status          string `json:"status"`
	Version         uint   `json:"version"`
	ManifestVersion uint   `json:"manifestVersion"`
}

func decodeCheck(t *testing.T, body []byte) checkRecord {
	t.Helper()
	var envelope struct {
		Data checkRecord `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Data.ID == 0 {
		t.Fatalf("decode check: %v body=%s", err, string(body))
	}
	return envelope.Data
}

func submitManifest(t *testing.T, engine http.Handler, token string, manifest record) record {
	t.Helper()
	response, body := request(t, engine, http.MethodPost, fmt.Sprintf("/api/manifests/%d/transition", manifest.ID), token, "gate-submit-"+manifest.Code, map[string]any{
		"status": "submitted", "expectedVersion": manifest.Version, "reason": "linked permits checked",
	})
	assertStatus(t, response, http.StatusOK)
	return decodeRecord(t, body)
}

func shipManifest(t *testing.T, engine http.Handler, token string, manifest record, requestID string) (*http.Response, []byte) {
	t.Helper()
	return request(t, engine, http.MethodPost, fmt.Sprintf("/api/manifests/%d/transition", manifest.ID), token, requestID, map[string]any{
		"status": "in_transit", "expectedVersion": manifest.Version, "reason": "现场发运确认",
	})
}

func decideCheck(t *testing.T, engine http.Handler, token string, check checkRecord, target, requestID string) checkRecord {
	t.Helper()
	response, body := request(t, engine, http.MethodPost, fmt.Sprintf("/api/checks/%d/transition", check.ID), token, requestID, map[string]any{
		"status": target, "expectedVersion": check.Version, "reason": "复核决定：" + target,
	})
	assertStatus(t, response, http.StatusOK)
	return decodeCheck(t, body)
}

func TestShipmentGateAlignsCheckVersionAndAudits(t *testing.T) {
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

	operator := login(t, engine, "operator")
	reviewer := login(t, engine, "reviewer")

	// 没有核验记录：发运必须被拦截并说明原因。
	response, body := request(t, engine, http.MethodPost, "/api/manifests", operator, "gate-create-1", manifestPayload("TM-GATE-001", "CP-002"))
	assertStatus(t, response, http.StatusCreated)
	manifest := submitManifest(t, engine, operator, decodeRecord(t, body))
	response, body = shipManifest(t, engine, operator, manifest, "gate-no-record")
	assertStatus(t, response, http.StatusUnprocessableEntity)
	if !bytes.Contains(body, []byte("没有核验记录")) {
		t.Fatalf("expected missing-record reason, got %s", string(body))
	}

	// 仍待处理：最新核验记下清单版本但未决定，拦截时给出核验编号。
	response, body = request(t, engine, http.MethodPost, "/api/checks", operator, "gate-check-create-1", checkPayload("CC-GATE-001", manifest.Code))
	assertStatus(t, response, http.StatusCreated)
	check := decodeCheck(t, body)
	if check.ManifestVersion != manifest.Version {
		t.Fatalf("check must record manifest version %d, got %d", manifest.Version, check.ManifestVersion)
	}
	response, body = shipManifest(t, engine, operator, manifest, "gate-pending")
	assertStatus(t, response, http.StatusUnprocessableEntity)
	if !bytes.Contains(body, []byte("CC-GATE-001")) || !bytes.Contains(body, []byte("仍待处理")) {
		t.Fatalf("expected pending reason with check code, got %s", string(body))
	}

	// 不通过：复核失败的决定必须拦住发运。
	check = decideCheck(t, engine, reviewer, check, "fail", "gate-decide-fail")
	response, body = shipManifest(t, engine, operator, manifest, "gate-fail")
	assertStatus(t, response, http.StatusUnprocessableEntity)
	if !bytes.Contains(body, []byte("CC-GATE-001")) || !bytes.Contains(body, []byte("不通过")) {
		t.Fatalf("expected fail reason with check code, got %s", string(body))
	}

	// 升级复核：升级中的核验同样不放行。
	check = decideCheck(t, engine, reviewer, check, "escalated", "gate-decide-escalated")
	response, body = shipManifest(t, engine, operator, manifest, "gate-escalated")
	assertStatus(t, response, http.StatusUnprocessableEntity)
	if !bytes.Contains(body, []byte("CC-GATE-001")) || !bytes.Contains(body, []byte("升级复核")) {
		t.Fatalf("expected escalated reason with check code, got %s", string(body))
	}

	// 同编号同版本最新决定为通过：放行并留下带核验依据的审计。
	response, body = request(t, engine, http.MethodPost, "/api/checks", operator, "gate-check-create-2", checkPayload("CC-GATE-002", manifest.Code))
	assertStatus(t, response, http.StatusCreated)
	passed := decideCheck(t, engine, reviewer, decodeCheck(t, body), "pass", "gate-decide-pass")
	if passed.ManifestVersion != manifest.Version {
		t.Fatalf("pass decision must record manifest version %d, got %d", manifest.Version, passed.ManifestVersion)
	}
	response, body = shipManifest(t, engine, operator, manifest, "gate-release")
	assertStatus(t, response, http.StatusOK)
	if released := decodeRecord(t, body); released.Status != "in_transit" {
		t.Fatalf("expected in_transit after release, got %+v", released)
	}

	// 版本不符：核验针对旧清单版本，即使存在记录也不放行。
	response, body = request(t, engine, http.MethodPost, "/api/manifests", operator, "gate-create-2", manifestPayload("TM-GATE-002", "CP-002"))
	assertStatus(t, response, http.StatusCreated)
	stale := decodeRecord(t, body)
	response, body = request(t, engine, http.MethodPost, "/api/checks", operator, "gate-check-create-3", checkPayload("CC-GATE-003", stale.Code))
	assertStatus(t, response, http.StatusCreated)
	staleCheck := decodeCheck(t, body)
	if staleCheck.ManifestVersion != 1 {
		t.Fatalf("check on draft manifest must record version 1, got %d", staleCheck.ManifestVersion)
	}
	stale = submitManifest(t, engine, operator, stale)
	response, body = shipManifest(t, engine, operator, stale, "gate-version-mismatch")
	assertStatus(t, response, http.StatusUnprocessableEntity)
	if !bytes.Contains(body, []byte("CC-GATE-003")) || !bytes.Contains(body, []byte("不符")) {
		t.Fatalf("expected version-mismatch reason with check code, got %s", string(body))
	}

	// 审计：每次拦截与成功放行都按请求编号留痕。
	response, body = request(t, engine, http.MethodGet, "/api/audits?page=1&pageSize=100", reviewer, "gate-audit-read", nil)
	assertStatus(t, response, http.StatusOK)
	for _, requestID := range []string{"gate-no-record", "gate-pending", "gate-fail", "gate-escalated", "gate-version-mismatch", "gate-release"} {
		if !bytes.Contains(body, []byte(requestID)) {
			t.Fatalf("expected audit entry for request %s: %s", requestID, string(body))
		}
	}
	if !bytes.Contains(body, []byte("shipment_blocked")) {
		t.Fatalf("expected shipment_blocked audit actions: %s", string(body))
	}
	if !bytes.Contains(body, []byte("CC-GATE-002")) || !bytes.Contains(body, []byte("发运放行")) {
		t.Fatalf("expected release audit to reference the authorizing check: %s", string(body))
	}
}
