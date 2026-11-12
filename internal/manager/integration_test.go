package manager

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yanqin001/repo-manager/sdk"
)

// transportFunc 让 SDK 和身份查询走可控的内存 HTTP 链路。
type transportFunc func(*http.Request) (*http.Response, error)

// RoundTrip 将 req 交给测试处理函数，并返回其响应或错误。
func (f transportFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type gitFixture struct {
	t               *testing.T
	realGit         string
	work, published string
}

// newGitFixture 使用真实 Git 数据和仅重写传输的包装器，测试不访问线上仓库。
func newGitFixture(t *testing.T) gitFixture {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	f := gitFixture{t: t, realGit: realGit, work: filepath.Join(root, "work"), published: filepath.Join(root, "upstream.git")}
	f.git(root, "init", "-b", "main", f.work)
	f.commit("initial")
	f.git(f.work, "branch", "topic")
	f.git(f.work, "tag", "-a", "v1", "-m", "初始版本")
	f.git(root, "clone", "--bare", f.work, f.published)

	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0755); err != nil {
		t.Fatal(err)
	}
	wrapper := `#!/usr/bin/env bash
args=()
network=
for arg in "$@"; do
	case "$arg" in
		http.proxy=http://proxy.invalid:1234) printf '模拟代理失败\n' >&2; exit 1 ;;
		ls-remote|fetch) network=yes; args+=("$arg") ;;
		https://gitee.com/*|https://github.com/*)
			if [ "$network" = yes ]; then args+=("$RM_TEST_UPSTREAM"); else args+=("$arg"); fi ;;
		*) args+=("$arg") ;;
	esac
done
exec "$RM_TEST_GIT" -c protocol.file.allow=always "${args[@]}"
`
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(wrapper), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("RM_TEST_GIT", realGit)
	t.Setenv("RM_TEST_UPSTREAM", f.published)
	return f
}

func (f gitFixture) git(dir string, args ...string) string {
	f.t.Helper()
	cmd := exec.Command(f.realGit, append([]string{"-c", "user.name=测试", "-c", "user.email=tests@example.test",
		"-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	cmd.Env = gitEnvironment()
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (f gitFixture) commit(content string) string {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.work, "tracked"), []byte(content+"\n"), 0644); err != nil {
		f.t.Fatal(err)
	}
	f.git(f.work, "add", "tracked")
	f.git(f.work, "commit", "-m", content)
	return f.git(f.work, "rev-parse", "HEAD")
}

func testConfig(t *testing.T) Config {
	t.Helper()
	root := t.TempDir()
	text := `local_repo_path: ` + filepath.Join(root, "source") + `
direct:
  proxy: http://proxy.invalid:1234
  max_concurrency: 2
rayfile:
  local_cmd_path: /test/ray-download
  remote_cmd_path: /test/ray-upload
  host: rayfile.invalid
  port: 32001
  user: test
  password: test-secret
  max_concurrency: 1
remote:
  host: git.invalid
  repo_path: ` + filepath.Join(root, "remote") + "\n"
	filename := filepath.Join(root, "test.yaml")
	if err := os.WriteFile(filename, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(filename)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func startTestService(t *testing.T, cfg Config) *Service {
	t.Helper()
	service, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	notFound := &http.Client{Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	service.identity.direct, service.identity.proxy = notFound, notFound
	return service
}

func sdkClient(t *testing.T, s *Service) *sdk.Client {
	t.Helper()
	handler := s.Handler()
	client, err := sdk.NewClient("http://repo-manager.test", &http.Client{Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		return recorder.Result(), nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func syncUsingSDK(t *testing.T, client *sdk.Client, raw string) sdk.Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result, err := client.Sync(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// TestDirectSynchronizesRefsAndRebuildsSymlinkTarget 验证同步所有引用和软链接目标重建。
func TestDirectSynchronizesRefsAndRebuildsSymlinkTarget(t *testing.T) {
	f := newGitFixture(t)
	cfg := testConfig(t)
	s := startTestService(t, cfg)
	client := sdkClient(t, s)
	upstream := "https://gitee.com/acme/project.git"
	domain := filepath.Join(cfg.LocalRepoPath, "gitee.com")
	target := filepath.Join(t.TempDir(), "existing")
	f.git(f.work, "clone", f.published, target)
	f.git(target, "remote", "set-url", "origin", upstream)
	if err := os.Mkdir(domain, 0755); err != nil {
		t.Fatal(err)
	}
	repoPath := filepath.Join(domain, "acme_project")
	if err := os.Symlink(target, repoPath); err != nil {
		t.Fatal(err)
	}
	first := syncUsingSDK(t, client, upstream)
	if first.LocalPath != repoPath || first.Method != "direct" {
		t.Fatalf("同步结果：%+v", first)
	}
	f.git(f.work, "branch", "-D", "topic")
	f.git(f.work, "checkout", "-b", "topic/child", "main")
	commit := f.commit("topic/child updated")
	f.git(f.work, "tag", "-a", "v2", "-m", "新的版本")
	f.git(f.work, "push", f.published, ":topic", ":refs/tags/v1")
	f.git(f.work, "push", f.published, "topic/child", "refs/tags/v2")
	f.git(f.published, "symbolic-ref", "HEAD", "refs/heads/topic/child")

	clean := syncUsingSDK(t, client, upstream)
	if clean.Commit != commit || clean.DefaultBranch != "topic/child" {
		t.Fatalf("干净仓库未同步分支重命名：%+v", clean)
	}
	// 用未提交文件触发重建，同时验证重建后保留原有软链接。
	if err := os.WriteFile(filepath.Join(target, "untracked"), []byte("discard"), 0644); err != nil {
		t.Fatal(err)
	}
	result := syncUsingSDK(t, client, upstream)
	if result.LocalPath != first.LocalPath || result.DefaultBranch != "topic/child" || result.Commit != commit {
		t.Fatalf("同步结果：%+v", result)
	}
	if link, err := os.Readlink(repoPath); err != nil || link != target {
		t.Fatalf("软链接为 %q，错误：%v", link, err)
	}
	if _, err := os.Stat(filepath.Join(target, "untracked")); !os.IsNotExist(err) {
		t.Fatalf("重建后保留了未跟踪文件：%v", err)
	}
	got := f.git(target, "for-each-ref", "--format=%(refname)", "refs/remotes/origin", "refs/tags")
	want := "refs/remotes/origin/HEAD\nrefs/remotes/origin/main\nrefs/remotes/origin/topic/child\nrefs/tags/v2"
	if got != want {
		t.Fatalf("实际引用：\n%s\n预期：\n%s", got, want)
	}
}

// TestRepositoryIdentityPreservesPathAcrossRename 验证迁移复用原目录、旧地址再利用及重启恢复。
func TestRepositoryIdentityPreservesPathAcrossRename(t *testing.T) {
	f := newGitFixture(t)
	cfg := testConfig(t)
	// 测试代理成功时用透明的传输包装器，仍保持国外仓库走 proxy 路线。
	cfg.Direct.Proxy = "http://pass-through.invalid:1234"
	s := startTestService(t, cfg)
	setIdentity := func(id int, name string) {
		s.identity.mu.Lock()
		clear(s.identity.cache)
		s.identity.mu.Unlock()
		s.identity.proxy = &http.Client{Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
			payload, err := json.Marshal(map[string]any{"id": id, "full_name": name})
			if err != nil {
				return nil, err
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(payload)))}, nil
		})}
	}
	client := sdkClient(t, s)
	setIdentity(100, "acme/project")
	first := syncUsingSDK(t, client, "https://github.com/acme/project")
	if first.Method != "proxy" {
		t.Fatalf("同步方式为 %s", first.Method)
	}
	setIdentity(100, "neworg/renamed")
	renamed := syncUsingSDK(t, client, "https://github.com/neworg/renamed")
	if renamed.LocalPath != first.LocalPath || renamed.CanonicalURL != "https://github.com/neworg/renamed" {
		t.Fatalf("迁移结果：%+v", renamed)
	}
	if got := f.git(renamed.LocalPath, "remote", "get-url", "origin"); got != "https://github.com/neworg/renamed" {
		t.Fatalf("迁移后的 origin 为 %q", got)
	}
	// 原地址被不同平台 ID 占用时，应分配独立目录。
	setIdentity(200, "acme/project")
	reused := syncUsingSDK(t, client, "https://github.com/acme/project")
	if reused.LocalPath == renamed.LocalPath {
		t.Fatalf("两个仓库共享了 %q", reused.LocalPath)
	}
	s.Close()
	reopened := startTestService(t, cfg)
	again := syncUsingSDK(t, sdkClient(t, reopened), "https://github.com/neworg/renamed")
	if again.LocalPath != renamed.LocalPath {
		t.Fatalf("重启后路径由 %q 变为 %q", renamed.LocalPath, again.LocalPath)
	}
}

// TestRayfileFallbackTransfersFullAndDeltaBundles 验证代理回退、增量校验和失败保留正式目录。
func TestRayfileFallbackTransfersFullAndDeltaBundles(t *testing.T) {
	f := newGitFixture(t)
	cfg := testConfig(t)
	s := startTestService(t, cfg)
	client := sdkClient(t, s)
	server := t.TempDir()
	var uploads []string
	corruptDelta := false
	failDownload := false
	value := func(args []string, flag string) string {
		if idx := slices.Index(args, flag); idx >= 0 && idx+1 < len(args) {
			return args[idx+1]
		}
		return ""
	}
	copyFile := func(source, dest string) error {
		data, err := os.ReadFile(source)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
			return err
		}
		return os.WriteFile(dest, data, 0600)
	}
	s.relay.remote = func(ctx context.Context, input, program string, args ...string) (string, error) {
		if program != cfg.Rayfile.RemoteCmdPath {
			return execute(ctx, "", program, args, nil, strings.NewReader(input))
		}
		if value(args, "-o") != "upload" || value(args, "-d") != "/data/code/github.com" {
			return "", fmt.Errorf("上传目标不正确")
		}
		source := value(args, "-s")
		name := filepath.Base(source)
		uploads = append(uploads, name)
		return "", copyFile(source, filepath.Join(server, "github.com", name))
	}
	s.relay.download = func(ctx context.Context, program string, args ...string) (string, error) {
		source := value(args, "-s")
		if program != cfg.Rayfile.LocalCmdPath || filepath.Dir(source) != "/data/code/github.com" {
			return "", fmt.Errorf("下载源不正确")
		}
		name := filepath.Base(source)
		serverPath := filepath.Join(server, "github.com", name)
		if value(args, "-o") == "remove" {
			return "", os.Remove(serverPath)
		}
		if failDownload {
			return "", fmt.Errorf("模拟下载失败：%s", cfg.Rayfile.Password)
		}
		dest := filepath.Join(value(args, "-d"), name)
		if err := copyFile(serverPath, dest); err != nil {
			return "", err
		}
		if corruptDelta && strings.HasSuffix(name, "_delta.bundle") {
			return "", os.WriteFile(dest, []byte("corrupted"), 0600)
		}
		return "", nil
	}
	upstream := "https://github.com/acme/project.git"
	first := syncUsingSDK(t, client, upstream)
	if first.Method != "rayfile" || len(uploads) != 1 || !strings.HasSuffix(uploads[0], "_full.bundle") {
		t.Fatalf("同步结果 %+v，上传 %v", first, uploads)
	}
	f.git(f.work, "checkout", "topic")
	topicCommit := f.commit("topic 2")
	f.git(f.work, "push", f.published, "topic")
	second := syncUsingSDK(t, client, upstream)
	if second.Commit != first.Commit || len(uploads) != 2 || !strings.HasSuffix(uploads[1], "_delta.bundle") ||
		f.git(second.LocalPath, "rev-parse", "origin/topic") != topicCommit {
		t.Fatalf("非默认分支增量同步失败，结果 %+v，上传 %v", second, uploads)
	}
	corruptDelta = true
	f.git(f.work, "checkout", "main")
	thirdCommit := f.commit("main 2")
	f.git(f.work, "push", f.published, "main")
	third := syncUsingSDK(t, client, upstream)
	if third.Commit != thirdCommit || len(uploads) != 4 || !strings.HasSuffix(uploads[2], "_delta.bundle") ||
		!strings.HasSuffix(uploads[3], "_full.bundle") {
		t.Fatalf("损坏增量包未回退到全量，结果 %+v，上传 %v", third, uploads)
	}
	failDownload = true
	f.commit("main 3")
	f.git(f.work, "push", f.published, "main")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := client.Sync(ctx, upstream)
	if err == nil || !strings.Contains(err.Error(), "雷速下载") || strings.Contains(err.Error(), cfg.Rayfile.Password) {
		t.Fatalf("下载失败结果：%v", err)
	}
	if got := f.git(third.LocalPath, "rev-parse", "HEAD"); got != thirdCommit {
		t.Fatalf("下载失败后正式目录被改为 %s", got)
	}
}

// TestOverlappingRequestsShareTask 验证 SDK 的重叠请求使用同一个在途任务。
func TestOverlappingRequestsShareTask(t *testing.T) {
	f := newGitFixture(t)
	cfg := testConfig(t)
	s := startTestService(t, cfg)
	client := sdkClient(t, s)
	requested := make(chan struct{}, 1)
	resume := make(chan struct{})
	s.identity.direct = &http.Client{Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
		requested <- struct{}{}
		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		case <-resume:
			return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader(""))}, nil
		}
	})}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	upstream := "https://gitee.com/acme/project.git"
	first, err := client.Submit(ctx, upstream)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-requested:
	}
	second, err := client.Submit(ctx, "https://gitee.com/acme/project")
	close(resume)
	if err != nil || first.ID != second.ID {
		t.Fatalf("任务 %q 和 %q 未合并，错误：%v", first.ID, second.ID, err)
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		task, err := client.GetTask(ctx, first.ID)
		if err != nil {
			t.Fatal(err)
		}
		if task.Status == sdk.Failed {
			t.Fatal(task.Error)
		}
		if task.Status == sdk.Succeeded {
			if task.Result.Commit != f.git(f.published, "rev-parse", "HEAD") {
				t.Fatalf("错误的同步结果：%+v", task.Result)
			}
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
}
