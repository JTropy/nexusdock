package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestParseEmbeddingResponseRespectsOpenAIIndexes(t *testing.T) {
	vectors, err := parseEmbeddingResponse([]byte(`{"data":[{"index":1,"embedding":[0,1]},{"index":0,"embedding":[1,0]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(vectors) != 2 || vectors[0][0] != 1 || vectors[1][1] != 1 {
		t.Fatalf("indexed vectors were not restored to request order: %#v", vectors)
	}
}

func TestParseEmbeddingResponseRejectsInvalidIndexes(t *testing.T) {
	tests := map[string]string{
		"mixed":     `{"data":[{"index":0,"embedding":[1,0]},{"embedding":[0,1]}]}`,
		"duplicate": `{"data":[{"index":0,"embedding":[1,0]},{"index":0,"embedding":[0,1]}]}`,
		"fraction":  `{"data":[{"index":0.5,"embedding":[1,0]}]}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := parseEmbeddingResponse([]byte(body)); err == nil {
				t.Fatal("invalid indexed embedding response was accepted")
			}
		})
	}
}

func TestReindexRejectsDimensionMismatch(t *testing.T) {
	embedding := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
			{"index": 0, "embedding": []float64{1, 0}},
			{"index": 1, "embedding": []float64{0, 1, 0}},
		}})
	}))
	defer embedding.Close()

	registry := NewRegistry(t.TempDir())
	ai := AIConfig{Enabled: true, Endpoint: embedding.URL, Model: "test-model", Timeout: time.Second}
	for _, id := range []string{"development.first", "development.second"} {
		template := testTemplate(id, "1.0.0")
		template.Status = StatusActive
		template.Hash = templateHash(template)
		if err := writeTemplateJSON(registry.templatePath("published", id, template.Version), template); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := registry.ReindexVectors(context.Background(), ai); err == nil || !strings.Contains(err.Error(), "dimension mismatch") {
		t.Fatalf("dimension mismatch was not rejected: %v", err)
	}
	if _, err := os.Stat(registry.vectorIndexPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid workflow vector index was written: %v", err)
	}
}

func TestVectorIndexInfoDistinguishesStaleAndInvalid(t *testing.T) {
	registry := NewRegistry(t.TempDir())
	ai := AIConfig{Enabled: true, Endpoint: "http://example.invalid", Model: "new-model"}
	stale := VectorIndex{Model: "old-model", Dimension: 1, UpdatedAt: time.Now().UTC(), Documents: map[string]VectorDocument{
		"development.demo@1.0.0": {
			ID: "development.demo", Version: "1.0.0", Hash: "sha256:test", Text: "demo", Vector: []float64{1}, UpdatedAt: time.Now().UTC(),
		},
	}}
	if err := writeTemplateJSON(registry.vectorIndexPath(), stale); err != nil {
		t.Fatal(err)
	}
	if status, count := registry.VectorIndexInfo(ai); status != VectorIndexStale || count != 0 {
		t.Fatalf("stale index status=%q count=%d", status, count)
	}

	if err := os.WriteFile(registry.vectorIndexPath(), []byte(`{"model":"new-model","dimension":2,"documents":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if status, count := registry.VectorIndexInfo(ai); status != VectorIndexInvalid || count != 0 {
		t.Fatalf("invalid index status=%q count=%d", status, count)
	}
}

func TestVectorScoresRejectQueryDimensionMismatch(t *testing.T) {
	embedding := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float64{{1, 0, 0}}})
	}))
	defer embedding.Close()

	registry := NewRegistry(t.TempDir())
	ai := AIConfig{Enabled: true, Endpoint: embedding.URL, Model: "test-model", Timeout: time.Second}
	index := VectorIndex{Model: "test-model", Dimension: 2, UpdatedAt: time.Now().UTC(), Documents: map[string]VectorDocument{
		"development.demo@1.0.0": {
			ID: "development.demo", Version: "1.0.0", Hash: "sha256:test", Text: "demo", Vector: []float64{1, 0}, UpdatedAt: time.Now().UTC(),
		},
	}}
	if err := writeTemplateJSON(registry.vectorIndexPath(), index); err != nil {
		t.Fatal(err)
	}
	loaded, err := registry.loadVectorIndex(ai.Model)
	if err != nil {
		t.Fatal(err)
	}
	if scores := vectorScores(context.Background(), ai, loaded, "demo", "DockMini", "development"); scores != nil {
		t.Fatalf("dimension-mismatched query produced scores: %#v", scores)
	}
}

// workflow embedding 请求必须携带运行时设置里保存的 API Key。
func TestEmbedTextsSendsRuntimeAPIKey(t *testing.T) {
	const token = "workflow-embedding-secret"
	var authorization string
	embedding := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"index": 0, "embedding": []float64{1, 0}}}})
	}))
	defer embedding.Close()

	vectors, err := embedTexts(t.Context(), AIConfig{
		Endpoint: embedding.URL,
		Model:    "test-embedding",
		APIKey:   token,
		Timeout:  time.Second,
	}, []string{"workflow text"})
	if err != nil {
		t.Fatal(err)
	}
	if len(vectors) != 1 || len(vectors[0]) != 2 {
		t.Fatalf("unexpected vectors: %#v", vectors)
	}
	if authorization != "Bearer "+token {
		t.Fatalf("workflow embedding authorization=%q", authorization)
	}
}

func TestVectorIndexValidationRejectsInconsistentDocument(t *testing.T) {
	index := VectorIndex{Model: "test-model", Dimension: 2, Documents: map[string]VectorDocument{
		"development.demo@1.0.0": {ID: "development.demo", Version: "1.0.0", Vector: []float64{1}},
	}}
	if err := validateVectorIndex(index, "test-model"); err == nil {
		t.Fatal("inconsistent workflow vector index was accepted")
	}
}
