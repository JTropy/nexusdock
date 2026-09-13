package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/uvwt/nexusdock/internal/workflow"
)

const workflowCompositionNextAction = "Combine these templates for the current user goal: prune irrelevant steps, deduplicate, order the remaining steps, and merge completion conditions. Then call task_manage create with source_template_ids, composed steps, and completion_conditions."

// callWorkflowTemplateManage 是集中式 MCP 工具 workflow_template_manage 的入口，
// 与 REST 端点共用同一个 internal/workflow.Registry。
func (s *Server) callWorkflowTemplateManage(ctx context.Context, args map[string]any) (map[string]any, error) {
	action := strings.ToLower(stringArgument(args, "action"))
	switch action {
	case "publish":
		var template workflow.Template
		raw, ok := args["template"].(map[string]any)
		if !ok {
			return nil, errors.New("template is required")
		}
		if err := decodeMap(raw, &template); err != nil {
			return nil, fmt.Errorf("decode workflow template: %w", err)
		}
		if _, exists := args["allow_long_template"]; exists {
			template.AllowLongTemplate = boolArgument(args, "allow_long_template")
		}
		if reason := stringArgument(args, "long_template_reason"); reason != "" {
			template.LongTemplateReason = reason
		}
		published, err := s.workflowRegistry.Publish(template)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"ok": true, "action": action, "template_id": published.ID,
			"template_summary": s.workflowTemplateSummary(published), "source": "nexus-registry",
		}, nil

	case "retire":
		id, version := stringArgument(args, "template_id"), stringArgument(args, "template_version")
		if id == "" || version == "" {
			return nil, errors.New("template_id and template_version are required")
		}
		retired, err := s.workflowRegistry.Retire(id, version)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"ok": true, "action": action, "template_id": retired.ID,
			"template_summary": s.workflowTemplateSummary(retired), "source": "nexus-registry",
		}, nil

	case "get":
		id := stringArgument(args, "template_id")
		if id == "" {
			return nil, errors.New("template_id is required")
		}
		var template workflow.Template
		var err error
		if version := stringArgument(args, "template_version"); version != "" {
			template, err = s.workflowRegistry.Get(id, version)
		} else {
			template, err = s.workflowRegistry.Active(id)
		}
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"ok": true, "action": action, "template": template,
			"template_summary": s.workflowTemplateSummary(template), "source": "nexus-registry",
		}, nil

	case "get_many":
		ids, err := workflowTemplateIDsArgument(args, "template_ids")
		if err != nil {
			return nil, err
		}
		if len(ids) < 2 || len(ids) > 3 {
			return nil, errors.New("template_ids must contain 2 or 3 distinct ids")
		}
		templates := make([]workflow.Template, 0, len(ids))
		for _, id := range ids {
			template, err := s.workflowRegistry.Active(id)
			if err != nil {
				return nil, err
			}
			templates = append(templates, template)
		}
		return map[string]any{
			"ok": true, "action": action, "templates": templates, "count": len(templates),
			"composition_required": true, "next_required_action": workflowCompositionNextAction,
			"source": "nexus-registry",
		}, nil

	case "list":
		status := workflow.Status(stringArgument(args, "template_status"))
		if status != "" && status != workflow.StatusActive && status != workflow.StatusRetired {
			return nil, errors.New("template_status must be active or retired")
		}
		templates, err := s.workflowRegistry.List(status)
		if err != nil {
			return nil, err
		}
		summaries := make([]workflowTemplateSummary, 0, len(templates))
		for _, template := range templates {
			summaries = append(summaries, s.workflowTemplateSummary(template))
		}
		if status == "" {
			summaries = currentWorkflowTemplates(summaries)
		}
		return map[string]any{
			"ok": true, "action": action, "templates": summaries, "count": len(summaries),
			"workflow_dir": s.workflowRegistry.Root(), "source": "nexus-registry",
		}, nil

	case "match":
		return s.workflowTemplateMatchResult(ctx, stringArgument(args, "goal"), stringArgument(args, "device"), stringArgument(args, "type"))

	case "vector_index":
		result, err := s.workflowTemplateVectorIndexResult()
		if err != nil {
			return nil, err
		}
		result["action"] = action
		if available, exists := result["available"]; exists {
			result["vector_index_available"] = available
			delete(result, "available")
		}
		return result, nil

	default:
		return nil, fmt.Errorf("unsupported workflow_template_manage action: %s", action)
	}
}

func workflowTemplateIDsArgument(args map[string]any, key string) ([]string, error) {
	raw, ok := args[key]
	if !ok {
		return nil, errors.New("template_ids are required")
	}
	var values []string
	if err := decodeMapValue(raw, &values); err != nil {
		return nil, errors.New("template_ids must be an array of strings")
	}
	seen := make(map[string]struct{}, len(values))
	ids := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		ids = append(ids, value)
	}
	return ids, nil
}

func decodeMapValue(value any, target any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, target)
}

// workflowTemplateMatchResult 是 REST match 端点与 MCP match 动作共用的响应组装，
// 业务打分在 internal/workflow，这里补充向量索引状态与给模型的推荐动作。
func (s *Server) workflowTemplateMatchResult(ctx context.Context, goal, device, taskType string) (map[string]any, error) {
	ai := s.workflowAIConfig()
	candidates, err := s.workflowRegistry.Match(ctx, ai, goal, device, taskType)
	if err != nil {
		return nil, err
	}
	vectorStatus, vectorItems := s.workflowRegistry.VectorIndexInfo(ai)
	root := s.workflowRegistry.Root()
	result := map[string]any{
		"ok": true, "action": "match", "candidates": candidates, "count": len(candidates),
		"workflow_dir": root, "root": root, "source": "nexus-registry",
		"vector_search_enabled": ai.VectorEnabled(), "vector_index_status": vectorStatus,
		"vector_index_items": vectorItems, "embedding_model": ai.Model,
	}
	for key, value := range workflowMatchRecommendation(candidates) {
		result[key] = value
	}
	return result, nil
}

func (s *Server) workflowTemplateVectorIndexResult() (map[string]any, error) {
	snapshot, err := s.workflowRegistry.VectorIndexSnapshot(s.workflowAIConfig())
	if err != nil {
		return nil, err
	}
	switch snapshot.Status {
	case workflow.VectorIndexNotConfigured:
		return map[string]any{"ok": true, "available": false, "source": "nexus-registry", "vector_index_status": workflow.VectorIndexNotConfigured}, nil
	case workflow.VectorIndexMissing:
		return map[string]any{"ok": true, "available": false, "source": "nexus-registry", "vector_index_status": workflow.VectorIndexMissing}, nil
	case workflow.VectorIndexStale:
		return map[string]any{"ok": true, "available": false, "source": "nexus-registry", "vector_index_status": workflow.VectorIndexStale, "embedding_model": snapshot.Model}, nil
	}
	updatedAt := ""
	if !snapshot.ModTime.IsZero() {
		updatedAt = snapshot.ModTime.UTC().Format(time.RFC3339Nano)
	}
	return map[string]any{
		"ok": true, "available": true, "source": "nexus-registry",
		"file_name": "vector-index.json", "path": "workflow-templates/vector-index.json",
		"size_bytes": snapshot.SizeBytes, "updated_at": updatedAt, "content": string(snapshot.Content),
		"vector_index_status": workflow.VectorIndexReady, "vector_index_items": snapshot.Items,
		"embedding_model": snapshot.Model, "dimension": snapshot.Dimension,
	}, nil
}

// workflowMatchRecommendation 把最高候选分折算成给模型/前端的推荐动作；
// 阈值属于响应语义，和打分业务一起演进，但只在边界输出。
func workflowMatchRecommendation(candidates []workflow.Candidate) map[string]any {
	best := 0
	if len(candidates) > 0 {
		best = candidates[0].Score
	}
	recommended := "plain_task"
	reason := "no active template is specific enough; create a plain recoverable task"
	if best >= 85 {
		recommended = "use_template"
		reason = "top candidate score is strong enough to select by default"
	} else if best >= 60 {
		recommended = "consider_template"
		reason = "top candidate is plausible but should be checked against the user goal"
	}
	return map[string]any{"recommended": recommended, "recommendation_reason": reason, "best_candidate_score": best, "score_thresholds": map[string]any{"use_template": 85, "consider_template": 60, "plain_task_below": 60}}
}
