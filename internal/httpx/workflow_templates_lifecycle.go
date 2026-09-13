package httpx

import (
	"sort"

	"github.com/uvwt/nexusdock/internal/workflow"
)

type workflowCounter struct {
	Versions int `json:"versions"`
	Active   int `json:"active"`
	Retired  int `json:"retired"`
}

func workflowTemplateCounters(items []workflowTemplateSummary) map[string]workflowCounter {
	counters := make(map[string]workflowCounter)
	seen := make(map[string]map[string]bool)
	for _, item := range items {
		if item.ID == "" {
			continue
		}
		if seen[item.ID] == nil {
			seen[item.ID] = make(map[string]bool)
		}
		counter := counters[item.ID]
		if !seen[item.ID][item.FileName] {
			counter.Versions++
			seen[item.ID][item.FileName] = true
		}
		switch item.Status {
		case "active":
			counter.Active++
		case "retired":
			counter.Retired++
		}
		counters[item.ID] = counter
	}
	return counters
}

func attachWorkflowTemplateCounters(summary *workflowTemplateSummary, all []workflowTemplateSummary) {
	counter := workflowTemplateCounters(all)[summary.ID]
	summary.VersionCount = counter.Versions
	summary.ActiveCount = counter.Active
	summary.RetiredCount = counter.Retired
	summary.HasConflict = counter.Active > 1
}

// currentWorkflowTemplates 把全量版本折叠成“每个模板一个当前版本”的视图：
// active 优先于 retired，其次取更高版本，最后按文件修改时间兜底。
func currentWorkflowTemplates(all []workflowTemplateSummary) []workflowTemplateSummary {
	byID := make(map[string][]workflowTemplateSummary)
	for _, item := range all {
		if item.ID == "" {
			continue
		}
		byID[item.ID] = append(byID[item.ID], item)
	}
	items := make([]workflowTemplateSummary, 0, len(byID))
	for _, versions := range byID {
		current := versions[0]
		for _, item := range versions {
			if workflowTemplateRank(item, current) {
				current = item
			}
		}
		attachWorkflowTemplateCounters(&current, all)
		items = append(items, current)
	}
	sortWorkflowTemplates(items)
	return items
}

func workflowTemplateRank(candidate, current workflowTemplateSummary) bool {
	candidateRank := workflowTemplateStatusRank(candidate.Status)
	currentRank := workflowTemplateStatusRank(current.Status)
	if candidateRank != currentRank {
		return candidateRank > currentRank
	}
	if cmp := workflow.CompareVersions(candidate.Version, current.Version); cmp != 0 {
		return cmp > 0
	}
	return candidate.UpdatedAt.After(current.UpdatedAt)
}

func workflowTemplateStatusRank(status string) int {
	if status == "active" {
		return 2
	}
	if status == "retired" {
		return 1
	}
	return 0
}

func sortWorkflowTemplates(items []workflowTemplateSummary) {
	sort.Slice(items, func(i, j int) bool {
		if items[i].ID != items[j].ID {
			return items[i].ID < items[j].ID
		}
		if cmp := workflow.CompareVersions(items[i].Version, items[j].Version); cmp != 0 {
			return cmp > 0
		}
		return items[i].UpdatedAt.After(items[j].UpdatedAt)
	})
}
