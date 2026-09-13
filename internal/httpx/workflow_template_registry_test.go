package httpx

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/uvwt/nexusdock/internal/config"
	"github.com/uvwt/nexusdock/internal/workflow"
)

func newTestWorkflowRegistry(dataDir string) *workflow.Registry {
	return workflow.NewRegistry(filepath.Join(dataDir, "workflow-templates"))
}

// REST 发布端点只做 JSON 映射：请求解码、业务发布、响应形状由这里钉住；
// 哈希一致性、退役回滚、权限等业务行为已随职责迁移到 internal/workflow 的测试。
func TestWorkflowTemplatePublishRouteReturnsPublishedTemplate(t *testing.T) {
	handler := newTestHandler(t, config.Config{NexusDataDir: t.TempDir()})
	response := publishWorkflowTemplateValue(t, handler, testWorkflowTemplate("development.demo", "1.0.0"))
	if response.Code != http.StatusOK {
		t.Fatalf("publish workflow status=%d body=%s", response.Code, response.Body.String())
	}
	var result struct {
		OK       bool              `json:"ok"`
		Template workflow.Template `json:"template"`
		Source   string            `json:"source"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode publish response: %v", err)
	}
	if !result.OK || result.Source != "nexus-registry" {
		t.Fatalf("publish response=%#v", result)
	}
	if result.Template.ID != "development.demo" || result.Template.Status != workflow.StatusActive || result.Template.Hash == "" {
		t.Fatalf("published template=%#v", result.Template)
	}

	duplicate := publishWorkflowTemplateValue(t, handler, testWorkflowTemplate("development.demo", "1.0.0"))
	if duplicate.Code != http.StatusConflict || !strings.Contains(duplicate.Body.String(), "WORKFLOW_VERSION_IMMUTABLE") {
		t.Fatalf("duplicate publish status=%d body=%s", duplicate.Code, duplicate.Body.String())
	}
}

func TestWorkflowTemplateRetireUsesExplicitRoute(t *testing.T) {
	dataDir := t.TempDir()
	handler := newTestHandler(t, config.Config{NexusDataDir: dataDir})
	publishWorkflowTemplate(t, handler, "development.demo", "1.0.0")

	response := doJSON(t, handler, http.MethodPost, "/v1/workflow-templates/development.demo/1.0.0/retire", `{}`)
	if response.Code != http.StatusOK {
		t.Fatalf("retire workflow status=%d body=%s", response.Code, response.Body.String())
	}
	registry := newTestWorkflowRegistry(dataDir)
	retired, err := registry.Get("development.demo", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if retired.Status != workflow.StatusRetired || retired.RetiredAt == nil {
		t.Fatalf("retired template state=%q retired_at=%v", retired.Status, retired.RetiredAt)
	}
}

func TestWorkflowTemplateLegacyLifecycleRoutesAreRemoved(t *testing.T) {
	handler := newTestHandler(t, config.Config{NexusDataDir: t.TempDir()})
	for _, path := range []string{
		"/v1/workflow-templates/drafts",
		"/v1/workflow-templates/development.demo/1.0.0/validate",
		"/v1/workflow-templates/development.demo/1.0.0/publish",
	} {
		response := doJSON(t, handler, http.MethodPost, path, `{}`)
		if response.Code != http.StatusNotFound && response.Code != http.StatusMethodNotAllowed {
			t.Fatalf("legacy lifecycle route %s status=%d body=%s", path, response.Code, response.Body.String())
		}
	}
}

func TestWorkflowTemplateReadRejectsExtraPathSegments(t *testing.T) {
	handler := newTestHandler(t, config.Config{NexusDataDir: t.TempDir()})
	publishWorkflowTemplate(t, handler, "development.demo", "1.0.0")

	response := doJSON(t, handler, http.MethodGet, "/v1/workflow-templates/development.demo/1.0.0/extra", "")
	if response.Code != http.StatusNotFound {
		t.Fatalf("extra path segment status=%d body=%s", response.Code, response.Body.String())
	}
}

func publishWorkflowTemplate(t *testing.T, handler http.Handler, id, version string) {
	t.Helper()
	response := publishWorkflowTemplateValue(t, handler, testWorkflowTemplate(id, version))
	if response.Code != http.StatusOK {
		t.Fatalf("publish workflow status=%d body=%s", response.Code, response.Body.String())
	}
}

func publishWorkflowTemplateValue(t *testing.T, handler http.Handler, template workflow.Template) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"template": template})
	if err != nil {
		t.Fatalf("marshal workflow template: %v", err)
	}
	return doJSON(t, handler, http.MethodPost, "/v1/workflow-templates/publish", string(payload))
}

func testWorkflowTemplate(id, version string) workflow.Template {
	return workflow.Template{
		ID:          id,
		Version:     version,
		Title:       "Demo workflow",
		Description: "Workflow registry behavior test.",
		Match: workflow.MatchRule{
			Keywords: []string{"demo"},
			Devices:  []string{"DockMini"},
			Type:     "development",
		},
		CompletionConditions: []string{"Registry state remains consistent."},
		Steps: []workflow.Step{
			{ID: "verify_registry", Title: "Verify registry state", Phase: "verify"},
		},
	}
}
