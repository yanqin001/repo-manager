// Package sdk 提供 repo-manager 的 Go 客户端。调用方应能访问服务返回的本地仓库目录。
package sdk

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Status 表示同步任务当前所处的阶段。
type Status string

const (
	// Queued 表示任务等待执行。
	Queued Status = "queued"
	// Running 表示任务已开始执行。
	Running Status = "running"
	// Succeeded 表示本地仓库已同步，可以使用 Result 中的路径。
	Succeeded Status = "succeeded"
	// Failed 表示同步失败，Task.Error 包含失败阶段。
	Failed Status = "failed"
)

// Result 记录一次成功同步的结果。空仓库的 DefaultBranch 和 Commit 为空。
type Result struct {
	LocalPath     string `json:"local_path"`     // 服务分配的仓库绝对路径，可能指向现有软链接。
	CanonicalURL  string `json:"canonical_url"`  // 当前已知的上游规范地址。
	DefaultBranch string `json:"default_branch"` // 检出的上游默认分支。
	Commit        string `json:"commit"`         // 默认分支检出的对象 ID。
	Method        string `json:"method"`         // direct、proxy 或 rayfile。
}

// Task 是服务端返回的任务快照。只有 Succeeded 状态包含 Result。
type Task struct {
	ID     string  `json:"id"`               // 由服务生成的任务 ID。
	Status Status  `json:"status"`           // 任务当前状态。
	Result *Result `json:"result,omitempty"` // 成功同步结果。
	Error  string  `json:"error,omitempty"`  // 失败阶段及原因。
}

// Client 调用 repo-manager HTTP API，可由多个 goroutine 共用。
type Client struct {
	base string
	http *http.Client
}

// NewClient 使用 baseURL 创建客户端。httpClient 为 nil 时使用带超时的默认客户端。
// baseURL 必须是服务的 HTTP(S) 地址，可以包含反向代理路径前缀。
func NewClient(baseURL string, httpClient *http.Client) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("repo-manager 服务地址无效")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &Client{base: strings.TrimRight(u.String(), "/"), http: httpClient}, nil
}

// Submit 为 repoURL 提交同步任务，返回当前状态和任务 ID。
// ctx 只控制 HTTP 请求，任务被接受后由服务端独立执行。
func (c *Client) Submit(ctx context.Context, repoURL string) (Task, error) {
	body, err := json.Marshal(struct {
		URL string `json:"url"`
	}{URL: repoURL})
	if err != nil {
		return Task{}, err
	}
	return c.request(ctx, http.MethodPost, "/v1/sync", bytes.NewReader(body))
}

// GetTask 使用 id 查询任务快照。任务不存在或已经过期时返回服务端错误。
func (c *Client) GetTask(ctx context.Context, id string) (Task, error) {
	if _, err := hex.DecodeString(id); len(id) != 32 || err != nil {
		return Task{}, fmt.Errorf("同步任务 ID 无效")
	}
	return c.request(ctx, http.MethodGet, "/v1/tasks/"+id, nil)
}

// Sync 提交 repoURL 并等待完成，返回可直接使用的本地路径及默认分支版本。
// ctx 取消时停止等待，已经提交的服务端任务仍会继续执行。
func (c *Client) Sync(ctx context.Context, repoURL string) (Result, error) {
	task, err := c.Submit(ctx, repoURL)
	if err != nil {
		return Result{}, err
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		switch task.Status {
		case Succeeded:
			if task.Result == nil {
				return Result{}, fmt.Errorf("任务 %s 缺少同步结果", task.ID)
			}
			return *task.Result, nil
		case Failed:
			return Result{}, fmt.Errorf("任务 %s 失败：%s", task.ID, task.Error)
		case Queued, Running:
		default:
			return Result{}, fmt.Errorf("未知任务状态 %q", task.Status)
		}
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case <-ticker.C:
			task, err = c.GetTask(ctx, task.ID)
			if err != nil {
				return Result{}, err
			}
		}
	}
}

func (c *Client) request(ctx context.Context, method, path string, body io.Reader) (Task, error) {
	var task Task
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return task, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return task, err
	}
	defer resp.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 != 2 {
		var failure struct{ Error string }
		_ = decoder.Decode(&failure)
		return task, fmt.Errorf("repo-manager HTTP %d：%s", resp.StatusCode, failure.Error)
	}
	return task, decoder.Decode(&task)
}
