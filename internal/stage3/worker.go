package stage3

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/uvwt/nexusdock/internal/agentdock"
	"github.com/uvwt/nexusdock/internal/recall"
	"github.com/uvwt/nexusdock/internal/workflow"
)

const (
	taskListPerNode   = 32
	taskDetailPerNode = 8
	lifecycleLimit    = 50
	workflowLimit     = 30
)

// WorkerConfig 是调度器每一轮需要的 Stage 3 配置切片。
// stage3 不能直接 import settings（settings 已依赖本包的默认超时常量，会构成 import cycle），
// 因此由组合根把 settings.RuntimeAIConfig 中的 Stage 3 字段映射为这里的具体结构。
type WorkerConfig struct {
	Enabled  bool
	Endpoint string
	Model    string
	APIKey   string
	Timeout  time.Duration
	Interval time.Duration
}

// Worker 是 Stage 3 进化分析的后台调度器：按配置周期汇总多设备事实，生成进化候选并提交 AgentDock。
// 它是应用级后台任务而不是 HTTP 职责，由组合根创建并随进程生命周期启动、取消；
// HTTP 层只保留设置端点的请求解析，并在设置保存成功后调用 Wake。
type Worker struct {
	logger *slog.Logger
	// config 每轮循环重新读取运行期 AI 设置；这样模型地址、模型名、密钥与间隔无需重启即可生效。
	config func(context.Context) (WorkerConfig, error)
	nodes  *agentdock.Store
	hub    *agentdock.Hub
	// memories 与 workflows 是快照的事实来源：本地 Recall 生命周期记录与已发布 Workflow 模板。
	memories  *recall.Store
	workflows *workflow.Registry
	wake      chan struct{}
}

func NewWorker(
	logger *slog.Logger,
	config func(context.Context) (WorkerConfig, error),
	nodes *agentdock.Store,
	hub *agentdock.Hub,
	memories *recall.Store,
	workflows *workflow.Registry,
) *Worker {
	return &Worker{
		logger: logger, config: config, nodes: nodes, hub: hub,
		memories: memories, workflows: workflows, wake: make(chan struct{}, 1),
	}
}

// Wake 非阻塞地唤醒调度器。缓冲为 1：连续多次设置变化只保留一次唤醒信号，
// 唤醒只触发重新读取配置，不会推迟原有的执行节奏。
func (w *Worker) Wake() {
	if w == nil {
		return
	}
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Run 阻塞运行调度循环直到 ctx 取消；由组合根在独立 goroutine 中启动。
func (w *Worker) Run(ctx context.Context) {
	w.loop(ctx, time.Now, newWorkerTimer, w.runConfigured)
}

// loop 的调度语义保持确定性且可测试：可运行配置先立即执行一次，
// 之后每次执行间隔都锚定在上一次尝试时间上；唤醒只重新加载配置，不会不断推迟计时。
func (w *Worker) loop(ctx context.Context, now func() time.Time, newTimer func(time.Duration) workerTimer, run func(context.Context, WorkerConfig)) {
	var lastAttempt time.Time
	wasRunnable := false
	for {
		cfg, err := w.config(ctx)
		if err != nil {
			// 读取设置失败通常是控制库瞬时故障；保持上一轮节奏不变，等待下一次设置变化或 ctx 结束。
			if w.logger != nil {
				w.logger.Warn("读取 Stage 3 运行设置失败，等待设置变化后重试", "error", err)
			}
			if !w.wait(ctx) {
				return
			}
			continue
		}
		runnable := cfg.Enabled && strings.TrimSpace(cfg.Endpoint) != "" && strings.TrimSpace(cfg.Model) != ""
		if !runnable {
			// 重新启用 Stage 3 属于新一轮可运行周期，应当立即执行一次。
			lastAttempt = time.Time{}
			wasRunnable = false
			if !w.wait(ctx) {
				return
			}
			continue
		}

		interval := cfg.Interval
		if interval < time.Hour {
			interval = time.Hour
		}
		if !wasRunnable || lastAttempt.IsZero() {
			wasRunnable = true
			run(ctx, cfg)
			lastAttempt = now()
			continue
		}

		wait := lastAttempt.Add(interval).Sub(now())
		if wait <= 0 {
			run(ctx, cfg)
			lastAttempt = now()
			continue
		}
		timer := newTimer(wait)
		select {
		case <-ctx.Done():
			timer.stop()
			return
		case <-w.wake:
			timer.stop()
			continue
		case <-timer.c:
			run(ctx, cfg)
			lastAttempt = now()
		}
	}
}

func (w *Worker) wait(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-w.wake:
		return true
	}
}

type workerTimer struct {
	c    <-chan time.Time
	stop func()
}

func newWorkerTimer(wait time.Duration) workerTimer {
	timer := time.NewTimer(wait)
	return workerTimer{
		c: timer.C,
		stop: func() {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		},
	}
}

func (w *Worker) runConfigured(ctx context.Context, cfg WorkerConfig) {
	client, err := NewClient(Config{
		Endpoint: cfg.Endpoint, Model: cfg.Model, APIKey: cfg.APIKey, Timeout: cfg.Timeout,
	})
	if err != nil {
		if w.logger != nil {
			w.logger.Warn("Stage 3 evolution skipped invalid model configuration", "error", err)
		}
		return
	}
	if err := w.run(ctx, client); err != nil && w.logger != nil {
		w.logger.Warn("Stage 3 evolution run failed", "error", err)
	}
}

func (w *Worker) run(ctx context.Context, client *Client) error {
	if client == nil {
		return fmt.Errorf("Stage 3 model client is nil")
	}
	snapshot, nodes, err := w.snapshot(ctx)
	if err != nil {
		return err
	}
	if len(nodes) == 0 || (len(snapshot.Tasks) == 0 && len(snapshot.Lifecycle) == 0 && len(snapshot.Workflows) == 0) {
		return nil
	}
	output, err := client.Generate(ctx, snapshot)
	if err != nil {
		return err
	}
	allowedEvidence := allowedEvidence(snapshot.Tasks)
	for _, candidate := range output.Candidates {
		candidate.EvidenceRefs = filterEvidence(candidate.EvidenceRefs, allowedEvidence)
		nodeID := targetNode(nodes, candidate.Device)
		payload := map[string]any{
			"intent": "propose",
			"candidate": map[string]any{
				"type": candidate.Type, "statement": candidate.Statement, "scope": candidate.Scope,
				"project": candidate.Project, "device": candidate.Device, "canonical_key": candidate.CanonicalKey,
				"tags": candidate.Tags,
			},
			"evidence_refs": candidate.EvidenceRefs,
			"rationale":     candidate.Rationale,
		}
		encoded, err := json.Marshal(payload)
		if err == nil {
			err = w.hub.RuntimeEvolve(ctx, nodeID, encoded)
		}
		if err != nil {
			if w.logger != nil {
				w.logger.Warn("Stage 3 proposal rejected by AgentDock", "node_id", nodeID, "candidate_type", candidate.Type, "error", err)
			}
			continue
		}
	}
	return nil
}

// snapshot 汇总 Stage 3 可用事实：本地生命周期记录、各启用 AgentDock 已定稿任务的评审结果、
// 已发布 Workflow 模板。敏感标签与 local_only 记录在进入快照前被剔除，模型侧还会再做一次脱敏。
func (w *Worker) snapshot(ctx context.Context) (Snapshot, []agentdock.Node, error) {
	if w.nodes == nil {
		return Snapshot{}, nil, fmt.Errorf("AgentDock node store unavailable")
	}
	nodes, err := w.nodes.List(ctx)
	if err != nil {
		return Snapshot{}, nil, err
	}
	enabled := make([]agentdock.Node, 0, len(nodes))
	for _, node := range nodes {
		if node.Enabled {
			enabled = append(enabled, node)
		}
	}
	snapshot := Snapshot{}

	records, err := w.memories.QueryLifecycle(recall.LifecycleQuery{Limit: lifecycleLimit})
	if err != nil {
		return Snapshot{}, nil, fmt.Errorf("query lifecycle for Stage 3: %w", err)
	}
	for _, record := range records {
		if strings.EqualFold(strings.TrimSpace(record.Scope), "local_only") || sensitiveTags(record.Tags) {
			continue
		}
		snapshot.Lifecycle = append(snapshot.Lifecycle, LifecycleFact{
			EvolutionID: record.EvolutionID, Type: record.Type, Statement: record.Statement, Scope: record.Scope,
			Project: record.Project, Device: record.Device, Status: record.Status, SupportCount: record.SupportCount,
			ContradictCount: record.ContradictCount, Tags: append([]string(nil), record.Tags...),
		})
	}

	for _, node := range enabled {
		tasks, taskErr := w.hub.RuntimeTasks(ctx, node.ID, taskListPerNode)
		if taskErr != nil {
			if w.logger != nil {
				w.logger.Debug("Stage 3 skipped unavailable AgentDock node", "node_id", node.ID, "error", taskErr)
			}
			continue
		}
		sort.SliceStable(tasks, func(i, j int) bool { return tasks[i].UpdatedAt > tasks[j].UpdatedAt })
		count := 0
		for _, summary := range tasks {
			if count >= taskDetailPerNode {
				break
			}
			if summary.ReviewStatus != "pass" && summary.ReviewStatus != "failed" {
				continue
			}
			detail, detailErr := w.hub.RuntimeTask(ctx, node.ID, summary.ID)
			if detailErr != nil {
				continue
			}
			if detail.FinalReview == nil || detail.FinalReview.ReviewRevision == "" {
				continue
			}
			finalReview := detail.FinalReview
			snapshot.Tasks = append(snapshot.Tasks, TaskFact{
				NodeID: node.ID, TaskID: summary.ID, Title: summary.Title, Goal: summary.Goal, Summary: summary.Summary,
				Status: summary.Status, ReviewStatus: summary.ReviewStatus, ReviewRevision: finalReview.ReviewRevision,
				VerifiedFacts: finalReview.VerifiedFacts, OpenRisks: finalReview.OpenRisks,
				MissingChecks: finalReview.MissingChecks, UpdatedAt: summary.UpdatedAt,
			})
			count++
		}
	}

	workflows, err := w.workflows.List(workflow.StatusActive)
	if err == nil {
		workflows = workflow.LatestVersions(workflows)
		if len(workflows) > workflowLimit {
			workflows = workflows[:workflowLimit]
		}
		for _, template := range workflows {
			snapshot.Workflows = append(snapshot.Workflows, WorkflowFact{
				ID: template.ID, Version: template.Version, Title: template.Title, Description: template.Description, Type: template.Match.Type,
			})
		}
	}
	return RedactSnapshot(snapshot), enabled, nil
}

func sensitiveTags(tags []string) bool {
	for _, tag := range tags {
		switch strings.ToLower(strings.TrimSpace(tag)) {
		case "sensitive", "local_only", "private", "secret":
			return true
		}
	}
	return false
}

// allowedEvidence 只允许模型引用快照中出现过的评审事实，防止幻觉 evidence ref 进入进化候选。
func allowedEvidence(tasks []TaskFact) map[string]bool {
	allowed := map[string]bool{}
	for _, task := range tasks {
		prefix := "task:" + task.TaskID + ":review:" + task.ReviewRevision
		for i := range task.VerifiedFacts {
			allowed[fmt.Sprintf("%s:verified:%d", prefix, i)] = true
		}
		for i := range task.OpenRisks {
			allowed[fmt.Sprintf("%s:risk:%d", prefix, i)] = true
		}
		for i := range task.MissingChecks {
			allowed[fmt.Sprintf("%s:missing:%d", prefix, i)] = true
		}
	}
	return allowed
}

func filterEvidence(refs []string, allowed map[string]bool) []string {
	out := make([]string, 0, len(refs))
	seen := map[string]bool{}
	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		if !allowed[ref] || seen[ref] {
			continue
		}
		seen[ref] = true
		out = append(out, ref)
	}
	return out
}

func targetNode(nodes []agentdock.Node, device string) string {
	device = strings.TrimSpace(device)
	for _, node := range nodes {
		if device != "" && strings.EqualFold(node.ID, device) {
			return node.ID
		}
	}
	return nodes[0].ID
}
