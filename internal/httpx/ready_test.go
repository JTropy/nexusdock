package httpx

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/uvwt/nexusdock/internal/config"
	"github.com/uvwt/nexusdock/internal/core"
	"github.com/uvwt/nexusdock/internal/recall"
)

func TestReadyReturnsOKWhenDatabaseAndRecallRootAreHealthy(t *testing.T) {
	h := newTestHandler(t, config.Config{})

	res := httptest.NewRecorder()
	h.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("ready status = %d body=%s", res.Code, res.Body.String())
	}
	var body struct {
		OK      bool              `json:"ok"`
		Checks  map[string]string `json:"checks"`
		Service string            `json:"service"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.OK || body.Service != "nexusdock" {
		t.Fatalf("ready body = %#v", body)
	}
}

func TestReadyReportsUnavailableDatabaseWith503(t *testing.T) {
	db, err := core.OpenSQLite(t.Context(), ":memory:", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := core.EnsureSchema(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	store, err := recall.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(config.Config{}, store, slog.Default(), WithSystemDatabase(db))
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	res := httptest.NewRecorder()
	server.Handler().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("ready status = %d body=%s", res.Code, res.Body.String())
	}
	var body struct {
		OK     bool              `json:"ok"`
		Checks map[string]string `json:"checks"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.OK || body.Checks["database"] != "unavailable" {
		t.Fatalf("ready body should report database failure: %#v", body)
	}
	if strings.Contains(res.Body.String(), "database is closed") {
		t.Fatalf("ready response leaked database error: %s", res.Body.String())
	}
}

func TestReadyReportsInaccessibleRecallRootWith503(t *testing.T) {
	db, err := core.OpenSQLite(t.Context(), ":memory:", 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := core.EnsureSchema(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	store, err := recall.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// 模拟生产上 Recall 卷被卸载或权限被改：根目录从进程视角消失后必须不再就绪。
	if err := os.Remove(store.Root()); err != nil {
		t.Fatal(err)
	}
	server := NewServer(config.Config{}, store, slog.Default(), WithSystemDatabase(db))

	res := httptest.NewRecorder()
	server.Handler().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("ready status = %d body=%s", res.Code, res.Body.String())
	}
	var body struct {
		OK     bool              `json:"ok"`
		Checks map[string]string `json:"checks"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.OK || body.Checks["recall"] != "unavailable" {
		t.Fatalf("ready body should report recall failure: %#v", body)
	}
	if strings.Contains(res.Body.String(), store.Root()) {
		t.Fatalf("ready response leaked recall root: %s", res.Body.String())
	}
}

// 进程存活但依赖不可用时，liveness 必须仍为 200，只有 readiness 变 503，
// 编排层据此区分"重启容器"与"摘流量"。
func TestHealthStaysAliveWhenReadyFails(t *testing.T) {
	db, err := core.OpenSQLite(t.Context(), ":memory:", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := core.EnsureSchema(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	store, err := recall.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(config.Config{}, store, slog.Default(), WithSystemDatabase(db))
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()

	ready := httptest.NewRecorder()
	handler.ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if ready.Code != http.StatusServiceUnavailable {
		t.Fatalf("ready status = %d", ready.Code)
	}
	health := httptest.NewRecorder()
	handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/health", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("health status = %d while not ready", health.Code)
	}
}
