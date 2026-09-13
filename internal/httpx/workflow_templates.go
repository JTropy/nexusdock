package httpx

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/uvwt/nexusdock/internal/workflow"
)

// workflowTemplateSummary 是模板在 HTTP/MCP 响应里的展示视图：
// 在业务模板之上补充文件名、相对路径与文件元数据，方便前端直接渲染列表。
type workflowTemplateSummary struct {
	ID           string    `json:"id"`
	Version      string    `json:"version"`
	Title        string    `json:"title"`
	Description  string    `json:"description,omitempty"`
	Status       string    `json:"status"`
	FileName     string    `json:"file_name"`
	Path         string    `json:"path"`
	SizeBytes    int64     `json:"size_bytes"`
	UpdatedAt    time.Time `json:"updated_at"`
	StepCount    int       `json:"step_count"`
	Keywords     []string  `json:"keywords,omitempty"`
	VersionCount int       `json:"version_count,omitempty"`
	ActiveCount  int       `json:"active_count,omitempty"`
	RetiredCount int       `json:"retired_count,omitempty"`
	HasConflict  bool      `json:"has_conflict,omitempty"`
}

func (s *Server) workflowTemplateSummary(t workflow.Template) workflowTemplateSummary {
	summary := workflowTemplateSummaryFromTemplate(t)
	s.attachWorkflowTemplateFileMetadata(&summary)
	return summary
}

func workflowTemplateSummaryFromTemplate(t workflow.Template) workflowTemplateSummary {
	fileName := t.ID + "@" + t.Version + ".json"
	return workflowTemplateSummary{ID: t.ID, Version: t.Version, Title: firstNonEmptyString(t.Title, t.ID), Description: t.Description, Status: string(t.Status), FileName: fileName, Path: filepath.ToSlash(filepath.Join("workflow-templates", "published", fileName)), StepCount: len(t.Steps), Keywords: t.Match.Keywords}
}

func (s *Server) attachWorkflowTemplateFileMetadata(summary *workflowTemplateSummary) {
	if summary == nil || summary.ID == "" || summary.Version == "" {
		return
	}
	summary.Path = filepath.ToSlash(filepath.Join("workflow-templates", "published", summary.FileName))
	info, err := os.Stat(s.workflowRegistry.PublishedFilePath(summary.ID, summary.Version))
	if err != nil {
		return
	}
	summary.SizeBytes = info.Size()
	summary.UpdatedAt = info.ModTime().UTC()
}

func templateSummaryMatches(summary workflowTemplateSummary, query string) bool {
	haystack := strings.ToLower(strings.Join([]string{summary.ID, summary.Version, summary.Title, summary.Description, summary.Status, summary.FileName}, " "))
	if strings.Contains(haystack, query) {
		return true
	}
	for _, keyword := range summary.Keywords {
		if strings.Contains(strings.ToLower(keyword), query) {
			return true
		}
	}
	return false
}

// workflowTemplateCompactList 是列表响应里的紧凑模板视图，字段是模板的业务字段全集。
func workflowTemplateCompactList(templates []workflow.Template) []map[string]any {
	out := make([]map[string]any, 0, len(templates))
	for _, t := range templates {
		out = append(out, map[string]any{"id": t.ID, "version": t.Version, "title": t.Title, "description": t.Description, "status": t.Status, "match": t.Match, "completion_conditions": t.CompletionConditions, "steps": t.Steps, "step_count": len(t.Steps), "hash": t.Hash, "published_at": t.PublishedAt, "retired_at": t.RetiredAt})
	}
	return out
}
