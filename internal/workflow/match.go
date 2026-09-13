package workflow

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Match 对当前 active 模板（每个 ID 只取最高版本）按目标做词法 + 向量打分，
// 返回按分数降序的候选。没有任何语义命中时退回“弱证据”候选，保证 match
// 总能给出可解释的下一步建议而不是空结果。
func (r *Registry) Match(ctx context.Context, ai AIConfig, goal, device, taskType string) ([]Candidate, error) {
	// 模板目录与向量索引文件在同一把锁内读取，保证 match 看到的模板集合
	// 与向量索引一致；embedding 网络调用必须放在锁外，避免阻塞发布/退役。
	r.mu.Lock()
	templates, err := r.listLocked(StatusActive)
	if err != nil {
		r.mu.Unlock()
		return nil, err
	}
	templates = LatestVersions(templates)
	generation := templateGeneration(templates)
	var index *VectorIndex
	if ai.VectorEnabled() && strings.TrimSpace(goal) != "" {
		if loaded, loadErr := r.loadVectorIndex(ai.Model, generation); loadErr == nil && len(loaded.Documents) > 0 {
			index = &loaded
		}
	}
	r.mu.Unlock()

	var scores map[string]float64
	if index != nil {
		scores = vectorScores(ctx, ai, *index, goal, device, taskType)
	}

	query := matchText(strings.Join([]string{goal, taskType, device}, " "))
	out := []Candidate{}
	fallback := []Candidate{}
	for _, t := range templates {
		score := 0
		semantic := false
		reasons := []string{}
		for _, keyword := range t.Match.Keywords {
			keyword = strings.TrimSpace(keyword)
			if keyword != "" && strings.Contains(query, matchText(keyword)) {
				if weakKeyword(keyword) {
					score += 15
					reasons = append(reasons, "context_keyword:"+keyword)
					continue
				}
				score += 15
				semantic = true
				reasons = append(reasons, "keyword:"+keyword)
			}
		}
		if containsHint([]string{t.Match.Type}, taskType) {
			score += 80
			semantic = true
			reasons = append(reasons, "type:"+taskType)
		}
		if vectorScore := scores[t.ID+"@"+t.Version]; vectorScore >= 0.55 {
			score += vectorBonus(vectorScore)
			semantic = true
			reasons = append(reasons, fmt.Sprintf("vector:%.2f", vectorScore))
		}
		if containsHint(t.Match.Devices, device) {
			score += 5
			reasons = append(reasons, "device:"+device)
		}
		if score > 0 && len(reasons) > 0 {
			c := Candidate{ID: t.ID, Version: t.Version, Score: score, Reason: strings.Join(reasons, ", ")}
			if semantic {
				out = append(out, c)
			} else {
				fallback = append(fallback, c)
			}
		}
	}
	if len(out) == 0 {
		out = fallback
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score == out[j].Score {
			if out[i].ID == out[j].ID {
				return CompareVersions(out[i].Version, out[j].Version) > 0
			}
			return out[i].ID < out[j].ID
		}
		return out[i].Score > out[j].Score
	})
	return out, nil
}

// weakKeyword 标记只代表产品/环境名的高频词：它们命中只算上下文线索，
// 不足以支撑语义判定，避免所有目标都命中同一个模板。
func weakKeyword(keyword string) bool {
	switch matchText(keyword) {
	case "agentdock", "nexus", "vitapulse":
		return true
	default:
		return false
	}
}

// matchText 归一化匹配文本：小写并去掉空白与“个”，让中英混排关键词可以子串比较。
func matchText(value string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(value) {
		if r <= ' ' || r == '个' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func containsHint(values []string, hint string) bool {
	hint = matchText(hint)
	if hint == "" {
		return false
	}
	for _, value := range values {
		if matchText(value) == hint {
			return true
		}
	}
	return false
}

// vectorBonus 把余弦相似度折算成与词法分数同一量级的加分档位。
func vectorBonus(score float64) int {
	if score >= 0.85 {
		return 50
	}
	if score >= 0.75 {
		return 35
	}
	if score >= 0.65 {
		return 25
	}
	return 15
}
