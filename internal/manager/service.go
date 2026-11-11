package manager

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/yanqin001/repo-manger/sdk"
)

const (
	// maxPending 限制排队和执行中的任务总数，避免调用突发时无限占用资源。
	maxPending = 256
	// maxTaskHistory 和 taskTTL 限制可查询的已完成任务数量和保存时间。
	maxTaskHistory = 8192
	taskTTL        = 24 * time.Hour
)

var errBusy = errors.New("同步队列已满")

type job struct {
	task      sdk.Task
	submitted time.Time
	finished  time.Time
}

// Service 管理同步任务和仓库目录。一个仓库根目录只能由一个 Service 写入。
type Service struct {
	cfg      Config
	registry *registry
	identity *identityResolver
	relay    relay
	ctx      context.Context
	cancel   context.CancelFunc
	stop     sync.Once
	wg       sync.WaitGroup
	direct   chan struct{}
	remote   chan struct{}
	workers  chan struct{}
	mu       sync.Mutex
	jobs     map[string]*job
	active   map[string]*job
	closed   bool
}

// New 使用 cfg 创建服务并锁定仓库根目录。cfg 应由 LoadConfig 生成。
// 配置无效或同一仓库根目录已经被占用时返回错误。
func New(cfg Config) (*Service, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	reg, err := openRegistry(cfg.LocalRepoPath)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	directLimit := min(maxPending, cfg.Direct.MaxConcurrency)
	remoteLimit := min(maxPending, cfg.Rayfile.MaxConcurrency)
	return &Service{
		cfg: cfg, registry: reg, identity: newIdentityResolver(cfg),
		relay: relay{cfg: cfg, remote: sshCommand(cfg.Remote.Host), download: localCommand},
		ctx:   ctx, cancel: cancel,
		direct: make(chan struct{}, directLimit), remote: make(chan struct{}, remoteLimit),
		workers: make(chan struct{}, min(maxPending, directLimit+remoteLimit)),
		jobs:    make(map[string]*job), active: make(map[string]*job),
	}, nil
}

// Close 停止接受新任务，取消并等待已有任务退出，然后释放仓库根目录锁。
func (s *Service) Close() {
	s.stop.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.cancel()
		s.mu.Unlock()
		s.wg.Wait()
		s.registry.lease.Close()
	})
}

func (s *Service) submit(repo repository) (sdk.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return sdk.Task{}, errBusy
	}
	if current, ok := s.active[repo.key]; ok {
		return current.task, nil
	}
	if len(s.active) >= maxPending {
		return sdk.Task{}, errBusy
	}
	now := time.Now()
	s.expireJobs(now)
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return sdk.Task{}, fmt.Errorf("生成同步任务 ID：%w", err)
	}
	id := hex.EncodeToString(token[:])
	j := &job{task: sdk.Task{ID: id, Status: sdk.Queued}, submitted: now}
	s.active[repo.key], s.jobs[id] = j, j
	s.wg.Add(1)
	go s.run(j, repo, id)
	return j.task, nil
}

func (s *Service) expireJobs(now time.Time) {
	var oldestID string
	var oldest time.Time
	for id, j := range s.jobs {
		if j.finished.IsZero() {
			continue
		}
		if now.Sub(j.finished) >= taskTTL {
			delete(s.jobs, id)
		} else if oldest.IsZero() || j.finished.Before(oldest) {
			oldest, oldestID = j.finished, id
		}
	}
	if len(s.jobs) >= maxTaskHistory {
		delete(s.jobs, oldestID)
	}
}

func (s *Service) get(id string) (sdk.Task, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok || (!j.finished.IsZero() && time.Since(j.finished) >= taskTTL) {
		return sdk.Task{}, false
	}
	return j.task, true
}

func (s *Service) run(j *job, repo repository, id string) {
	defer s.wg.Done()
	ctx, cancel := context.WithDeadline(s.ctx, j.submitted.Add(s.cfg.JobTimeout))
	defer cancel()
	var result sdk.Result
	err := withSlot(ctx, s.workers, func() error {
		s.mu.Lock()
		j.task.Status = sdk.Running
		s.mu.Unlock()
		var syncErr error
		result, syncErr = s.synchronize(ctx, repo, id)
		return syncErr
	})

	now := time.Now()
	s.mu.Lock()
	delete(s.active, repo.key)
	j.finished = now
	j.task.Status, j.task.Result = sdk.Succeeded, &result
	message := ""
	if err != nil {
		message = redactError(err, s.cfg)
		j.task.Status, j.task.Result, j.task.Error = sdk.Failed, nil, message
	}
	s.mu.Unlock()
	if err != nil {
		slog.Warn("仓库同步失败", "task", id, "url", repo.url, "error", message)
	} else {
		slog.Info("仓库同步完成", "task", id, "url", result.CanonicalURL, "method", result.Method,
			"commit", result.Commit, "duration", now.Sub(j.submitted))
	}
}

func (s *Service) synchronize(ctx context.Context, request repository, id string) (sdk.Result, error) {
	identity := s.identity.resolve(ctx, request)
	rel, err := s.registry.reserve(ctx, request, identity)
	if err != nil {
		return sdk.Result{}, fmt.Errorf("分配仓库目录：%w", err)
	}
	// 不同别名在核实身份前可能提交为不同任务，最终按同一个正式目录串行同步。
	rec, release, err := s.registry.acquire(ctx, rel)
	if err != nil {
		return sdk.Result{}, err
	}
	defer release()
	repo, err := parseRepository(rec.URL)
	if err != nil {
		return sdk.Result{}, err
	}
	local, err := inspectLocal(ctx, filepath.Join(s.cfg.LocalRepoPath, rel), rec)
	if err != nil {
		return sdk.Result{}, fmt.Errorf("检查本地仓库：%w", err)
	}
	var current snapshot
	method, proxy := "direct", ""
	if !repo.domestic() {
		method, proxy = "proxy", s.cfg.Direct.Proxy
	}
	upstreamFailure := false
	err = withSlot(ctx, s.direct, func() error {
		if method == "proxy" && proxy == "" {
			upstreamFailure = true
			return fmt.Errorf("未配置国外仓库代理")
		}
		gitCtx := ctx
		if method == "proxy" {
			timeout := s.cfg.Direct.ProxyTimeout
			if deadline, ok := ctx.Deadline(); ok {
				// 为 SSH 和雷速保留任务时间，防止慢代理耗尽整个截止时间。
				timeout = min(timeout, time.Until(deadline)/2)
			}
			var cancel context.CancelFunc
			gitCtx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
		}
		var gitErr error
		current, upstreamFailure, gitErr = synchronizeGit(gitCtx, repo, local, proxy)
		if gitErr != nil && gitCtx.Err() != nil && ctx.Err() == nil {
			upstreamFailure = true
		}
		return gitErr
	})
	if err != nil && !repo.domestic() && upstreamFailure && ctx.Err() == nil {
		proxyErr := err
		method = "rayfile"
		err = withSlot(ctx, s.remote, func() error {
			var relayErr error
			current, relayErr = s.relay.synchronize(ctx, repo, rec, rel, id, local)
			return relayErr
		})
		if err != nil {
			return sdk.Result{}, fmt.Errorf("代理拉取失败：%v；远端雷速回退失败：%w", proxyErr, err)
		}
	}
	if err != nil {
		return sdk.Result{}, fmt.Errorf("%s 同步失败：%w", method, err)
	}
	return sdk.Result{
		LocalPath: local.path, CanonicalURL: repo.url,
		DefaultBranch: current.branch, Commit: current.commit(), Method: method,
	}, nil
}

func withSlot(ctx context.Context, slots chan struct{}, work func() error) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case slots <- struct{}{}:
		defer func() { <-slots }()
		if err := ctx.Err(); err != nil {
			return err
		}
		return work()
	}
}

func synchronizeGit(ctx context.Context, repo repository, local localRepo, proxy string) (snapshot, bool, error) {
	git := localGit(proxy)
	advertised, err := discover(ctx, git, repo.cloneURL)
	if err != nil {
		return snapshot{}, true, err
	}
	if local.matches(ctx, repo, advertised) {
		return advertised, false, nil
	}
	stage, cleanup, err := local.newStage(ctx, advertised.oidSize)
	if err != nil {
		return snapshot{}, false, err
	}
	defer cleanup()
	current, err := fetchSnapshot(ctx, git, stage, repo.cloneURL, advertised.branch)
	if err != nil {
		return current, true, err
	}
	if err := publishGit(ctx, stage, repo, current); err != nil {
		return current, false, err
	}
	return current, false, local.install(ctx, stage)
}
