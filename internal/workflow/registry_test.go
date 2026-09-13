package workflow

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPublishRetiresOlderVersionAndKeepsHashesConsistent(t *testing.T) {
	registry := NewRegistry(t.TempDir())
	if _, err := registry.Publish(testTemplate("development.demo", "1.0.0")); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Publish(testTemplate("development.demo", "1.1.0")); err != nil {
		t.Fatal(err)
	}

	retired, err := registry.Get("development.demo", "1.0.0")
	if err != nil {
		t.Fatalf("load retired template: %v", err)
	}
	if retired.Status != StatusRetired || retired.RetiredAt == nil {
		t.Fatalf("older template was not retired: status=%q retired_at=%v", retired.Status, retired.RetiredAt)
	}
	if retired.Hash != templateHash(retired) {
		t.Fatalf("retired template hash is stale: got %q want %q", retired.Hash, templateHash(retired))
	}

	active, err := registry.Get("development.demo", "1.1.0")
	if err != nil {
		t.Fatalf("load active template: %v", err)
	}
	if active.Status != StatusActive || active.Hash != templateHash(active) {
		t.Fatalf("active template is inconsistent: status=%q hash=%q want_hash=%q", active.Status, active.Hash, templateHash(active))
	}
}

func TestPublishNormalizesCallerLifecycleMetadata(t *testing.T) {
	registry := NewRegistry(t.TempDir())
	template := testTemplate("development.demo", "1.0.0")
	template.Status = StatusRetired
	template.Hash = "sha256:caller-controlled"
	now := time.Now().UTC().Add(-time.Hour)
	template.RetiredAt = &now

	published, err := registry.Publish(template)
	if err != nil {
		t.Fatal(err)
	}
	if published.Status != StatusActive || published.RetiredAt != nil || published.PublishedAt == nil {
		t.Fatalf("publish did not normalize lifecycle metadata: %#v", published)
	}
	if published.Hash != templateHash(published) || published.Hash == "sha256:caller-controlled" {
		t.Fatalf("publish hash=%q is not server generated", published.Hash)
	}
}

func TestPublishFailureKeepsCurrentVersionActive(t *testing.T) {
	registry := NewRegistry(t.TempDir())
	current := testTemplate("development.demo", "1.0.0")
	current.Status = StatusActive
	current.Hash = templateHash(current)
	if err := writeTemplateJSON(registry.templatePath("published", current.ID, current.Version), current); err != nil {
		t.Fatal(err)
	}
	next := testTemplate(current.ID, "1.1.0")
	next.Status = StatusActive
	next.Hash = templateHash(next)

	failedPath := registry.templatePath("published", next.ID, next.Version)
	code, err := registry.publishWithWriter(next, time.Now().UTC(), func(path string, value any) error {
		if path == failedPath {
			return errors.New("injected publish failure")
		}
		return writeTemplateJSON(path, value)
	})
	if err == nil || code != "WORKFLOW_PUBLISH_FAILED" {
		t.Fatalf("publish result code=%q err=%v", code, err)
	}
	reloaded, err := registry.Get(current.ID, current.Version)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Status != StatusActive {
		t.Fatalf("current template status=%q", reloaded.Status)
	}
	if _, err := os.Stat(failedPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial new template remains: %v", err)
	}
}

func TestRetirementFailureRollsBackAllVersions(t *testing.T) {
	registry := NewRegistry(t.TempDir())
	for _, version := range []string{"0.9.0", "1.0.0"} {
		current := testTemplate("development.demo", version)
		current.Status = StatusActive
		current.Hash = templateHash(current)
		if err := writeTemplateJSON(registry.templatePath("published", current.ID, version), current); err != nil {
			t.Fatal(err)
		}
	}
	next := testTemplate("development.demo", "1.1.0")
	next.Status = StatusActive
	next.Hash = templateHash(next)
	failedPath := registry.templatePath("published", next.ID, "1.0.0")

	code, err := registry.publishWithWriter(next, time.Now().UTC(), func(path string, value any) error {
		template, ok := value.(Template)
		if path == failedPath && ok && template.Status == StatusRetired {
			return errors.New("injected retirement failure")
		}
		return writeTemplateJSON(path, value)
	})
	if err == nil || code != "WORKFLOW_RETIRE_OLD_FAILED" {
		t.Fatalf("publish result code=%q err=%v", code, err)
	}
	for _, version := range []string{"0.9.0", "1.0.0"} {
		reloaded, loadErr := registry.Get(next.ID, version)
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		if reloaded.Status != StatusActive {
			t.Fatalf("template %s status=%q after rollback", version, reloaded.Status)
		}
	}
	if _, err := os.Stat(registry.templatePath("published", next.ID, next.Version)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("new template remains after retirement rollback: %v", err)
	}
}

func TestGetReportsCorruptPublishedVersion(t *testing.T) {
	registry := NewRegistry(t.TempDir())
	template := testTemplate("development.demo", "1.0.0")
	publishedPath := registry.templatePath("published", template.ID, template.Version)
	if err := os.MkdirAll(filepath.Dir(publishedPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(publishedPath, []byte(`{"id":"development.demo","version":"1.0.0","unknown":true}`), 0o600); err != nil {
		t.Fatalf("write corrupt published template: %v", err)
	}

	_, err := registry.Get(template.ID, template.Version)
	if err == nil || !strings.Contains(err.Error(), "published") {
		t.Fatalf("corrupt published template was not reported: %v", err)
	}
}

func TestRegistryFilesRemainPrivate(t *testing.T) {
	registry := NewRegistry(t.TempDir())
	if err := registry.ensureDirs(); err != nil {
		t.Fatalf("ensure registry dirs: %v", err)
	}
	info, err := os.Stat(filepath.Join(registry.Root(), "published"))
	if err != nil {
		t.Fatalf("stat published: %v", err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("published permissions=%#o want 0700", info.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(registry.Root(), "drafts")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("drafts directory should not be created: %v", err)
	}
}

func testTemplate(id, version string) Template {
	return Template{
		ID:          id,
		Version:     version,
		Title:       "Demo workflow",
		Description: "Workflow registry behavior test.",
		Match: MatchRule{
			Keywords: []string{"demo"},
			Devices:  []string{"DockMini"},
			Type:     "development",
		},
		CompletionConditions: []string{"Registry state remains consistent."},
		Steps: []Step{
			{ID: "verify_registry", Title: "Verify registry state", Phase: "verify"},
		},
	}
}
