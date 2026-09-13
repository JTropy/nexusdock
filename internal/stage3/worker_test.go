package stage3

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// configSource 模拟运行期设置存储：循环每轮读取一次，测试中可以安全地修改配置或注入读取失败。
type configSource struct {
	mu   sync.Mutex
	cfg  WorkerConfig
	fail bool
}

func (s *configSource) load(context.Context) (WorkerConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return WorkerConfig{}, errors.New("settings store unavailable")
	}
	return s.cfg, nil
}

func (s *configSource) update(fn func(*WorkerConfig)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.cfg)
}

func TestWorkerSchedulerRunsImmediatelyAndWakeKeepsOriginalDeadline(t *testing.T) {
	base := time.Date(2026, 8, 16, 6, 0, 0, 0, time.UTC)
	var nowNanos atomic.Int64
	nowNanos.Store(base.UnixNano())
	now := func() time.Time { return time.Unix(0, nowNanos.Load()).UTC() }
	source := &configSource{cfg: WorkerConfig{Enabled: true, Endpoint: "http://model.invalid", Model: "test", Interval: 2 * time.Hour}}
	worker := &Worker{config: source.load, wake: make(chan struct{}, 1)}
	runs := make(chan WorkerConfig, 8)
	delays := make(chan time.Duration, 8)
	ticks := make(chan time.Time, 8)
	newTimer := func(wait time.Duration) workerTimer {
		delays <- wait
		return workerTimer{c: ticks, stop: func() {}}
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go worker.loop(ctx, now, newTimer, func(_ context.Context, got WorkerConfig) { runs <- got })

	select {
	case <-runs:
	case <-time.After(time.Second):
		t.Fatal("Stage 3 did not run immediately on startup")
	}
	if wait := <-delays; wait != 2*time.Hour {
		t.Fatalf("initial wait = %s", wait)
	}
	nowNanos.Store(base.Add(30 * time.Minute).UnixNano())
	worker.Wake()
	if wait := <-delays; wait != 90*time.Minute {
		t.Fatalf("wait after wake = %s", wait)
	}
	select {
	case <-runs:
		t.Fatal("config wake triggered an unintended immediate run")
	default:
	}
	ticks <- now()
	select {
	case <-runs:
	case <-time.After(time.Second):
		t.Fatal("scheduled Stage 3 run did not execute")
	}
}

func TestWorkerSchedulerRunsImmediatelyWhenReenabled(t *testing.T) {
	source := &configSource{cfg: WorkerConfig{Enabled: false, Endpoint: "http://model.invalid", Model: "test", Interval: 2 * time.Hour}}
	worker := &Worker{config: source.load, wake: make(chan struct{}, 1)}
	runs := make(chan struct{}, 2)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go worker.loop(ctx, time.Now, func(time.Duration) workerTimer {
		return workerTimer{c: make(chan time.Time), stop: func() {}}
	}, func(context.Context, WorkerConfig) { runs <- struct{}{} })

	// 模拟设置页保存后重新启用 Stage 3 并唤醒调度器：应立即执行一次，无需重启进程。
	source.update(func(cfg *WorkerConfig) { cfg.Enabled = true })
	worker.Wake()
	select {
	case <-runs:
	case <-time.After(time.Second):
		t.Fatal("Stage 3 did not run immediately after being re-enabled")
	}
}

func TestWorkerSchedulerRetriesConfigLoadWithoutWake(t *testing.T) {
	source := &configSource{cfg: WorkerConfig{Enabled: true, Endpoint: "http://model.invalid", Model: "test", Interval: 2 * time.Hour}, fail: true}
	worker := &Worker{config: source.load, wake: make(chan struct{}, 1)}
	runs := make(chan struct{}, 2)
	timersStarted := make(chan time.Duration, 2)
	ticks := make(chan time.Time, 2)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go worker.loop(ctx, time.Now, func(wait time.Duration) workerTimer {
		timersStarted <- wait
		return workerTimer{c: ticks, stop: func() {}}
	}, func(context.Context, WorkerConfig) { runs <- struct{}{} })

	// 设置读取失败期间不能带着空配置执行进化分析，并且必须安排自动重试。
	select {
	case wait := <-timersStarted:
		if wait != configRetryInterval {
			t.Fatalf("config retry wait=%s, want %s", wait, configRetryInterval)
		}
	case <-time.After(time.Second):
		t.Fatal("Stage 3 did not schedule config retry")
	}
	select {
	case <-runs:
		t.Fatal("Stage 3 ran although settings could not be loaded")
	default:
	}

	// 控制库恢复后不调用 Wake，仅触发重试计时器，Worker 也应自行恢复执行。
	source.mu.Lock()
	source.fail = false
	source.mu.Unlock()
	ticks <- time.Now()
	select {
	case <-runs:
	case <-time.After(time.Second):
		t.Fatal("Stage 3 did not recover after automatic config retry")
	}
}
