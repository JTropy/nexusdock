package workflow

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// embedding 服务挂起时取消请求，match 仍应及时返回并给出词法候选，
// 向量检索只是加分项，不允许阻塞或阻断主流程。
func TestMatchReturnsAfterClientCancellation(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var startedOnce sync.Once
	embedding := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startedOnce.Do(func() { close(started) })
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(embedding.Close)
	t.Cleanup(func() { close(release) })

	registry := NewRegistry(t.TempDir())
	ai := AIConfig{Enabled: true, Endpoint: embedding.URL, Model: "test-model", Timeout: 5 * time.Second}
	if _, err := registry.Publish(testTemplate("development.demo", "1.0.0")); err != nil {
		t.Fatal(err)
	}
	templates, err := registry.List(StatusActive)
	if err != nil {
		t.Fatal(err)
	}
	index := VectorIndex{
		Model:      "test-model",
		Generation: templateGeneration(LatestVersions(templates)),
		Dimension:  1,
		UpdatedAt:  time.Now().UTC(),
		Documents: map[string]VectorDocument{
			"development.demo@1.0.0": {ID: "development.demo", Version: "1.0.0", Hash: "sha256:test", Text: "demo", Vector: []float64{1}, UpdatedAt: time.Now().UTC()},
		},
	}
	if err := writeTemplateJSON(registry.vectorIndexPath(), index); err != nil {
		t.Fatalf("write vector index: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var candidates []Candidate
	var matchErr error
	go func() {
		candidates, matchErr = registry.Match(ctx, ai, "demo", "DockMini", "development")
		close(done)
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("embedding request did not start")
	}
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("workflow match did not finish after client cancellation")
	}
	if matchErr != nil || len(candidates) == 0 {
		t.Fatalf("match after cancellation: candidates=%#v err=%v", candidates, matchErr)
	}
	for _, candidate := range candidates {
		if candidate.ID == "development.demo" && strings.HasPrefix(candidate.Reason, "vector:") {
			t.Fatalf("cancelled vector query should not contribute a vector reason: %q", candidate.Reason)
		}
	}
}
