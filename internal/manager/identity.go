package manager

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

var (
	// segmentPattern 约束会进入本地、SSH 和雷速路径的仓库名称。
	segmentPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	// domainLabelPattern 禁止域名分段以连字符开头或结尾。
	domainLabelPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

func validSegment(s string) bool {
	return s != "." && s != ".." && !strings.HasPrefix(s, "-") && segmentPattern.MatchString(s)
}

func validDomain(host string) bool {
	if len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if !domainLabelPattern.MatchString(label) {
			return false
		}
	}
	return true
}

// repository 同时保存远端 URL 的路径大小写和用于去重的规范键。
type repository struct {
	key      string
	host     string
	name     string
	url      string
	cloneURL string
	apiPath  string
}

func parseRepository(raw string) (repository, error) {
	invalid := fmt.Errorf("仓库地址必须是有效的公开 Git HTTP(S) 或 git@host:path 地址")
	raw = strings.TrimSpace(raw)
	// git@ 形式按同一个公开 HTTPS 仓库处理，代理和远端路线共用 HTTP 传输。
	if strings.HasPrefix(raw, "git@") {
		host, repoPath, ok := strings.Cut(strings.TrimPrefix(raw, "git@"), ":")
		if !ok || !validDomain(strings.ToLower(host)) {
			return repository{}, invalid
		}
		raw = "https://" + host + "/" + repoPath
	}
	u, err := url.Parse(raw)
	if err != nil || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		u.Opaque != "" || u.Hostname() == "" {
		return repository{}, invalid
	}
	if u.Scheme == "ssh" && u.User != nil && u.User.String() == "git" && u.Port() == "" {
		u.Scheme, u.User = "https", nil
	}
	host := strings.ToLower(u.Hostname())
	if !validDomain(host) || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return repository{}, invalid
	}
	u.Host = strings.ToLower(u.Host)
	if (u.Scheme == "https" && u.Port() == "443") || (u.Scheme == "http" && u.Port() == "80") {
		u.Host = host
	}
	u.Path = strings.TrimRight(u.Path, "/")
	cloneURL := u.String()
	u.Path = strings.TrimSuffix(u.Path, ".git")
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(parts) < 2 || len(u.Path) > 2048 {
		return repository{}, invalid
	}
	for _, part := range parts {
		if !validSegment(part) {
			return repository{}, invalid
		}
	}
	name := strings.Join(parts, "_")
	if len(name) > 190 {
		name = name[:170] + "__" + shortHash(u.Host+u.Path)
	}
	key := u.Host + u.Path
	if host == "github.com" {
		key = strings.ToLower(key)
	}
	repo := repository{key: key, host: host, name: name, url: u.String(), cloneURL: cloneURL}
	if len(parts) == 2 && u.Port() == "" {
		repo.apiPath = strings.Join(parts, "/")
	}
	return repo, nil
}

func (r repository) domestic() bool {
	return r.host == "gitee.com" || r.host == "gitcode.com"
}

// repoIdentity 的键以平台隔离，相同数字 ID 在不同平台表示不同仓库。
type repoIdentity struct {
	key       string
	canonical repository
}

type identityEntry struct {
	identity repoIdentity
	expires  time.Time
}

type identityResolver struct {
	direct, proxy *http.Client
	tokens        map[string]string
	mu            sync.Mutex
	cache         map[string]identityEntry
}

func newIdentityResolver(cfg Config) *identityResolver {
	newClient := func(proxy string) *http.Client {
		transport := &http.Transport{
			DialContext:         (&net.Dialer{Timeout: 4 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout: 4 * time.Second,
			IdleConnTimeout:     60 * time.Second,
		}
		if proxy != "" {
			u, _ := url.Parse(proxy)
			transport.Proxy = http.ProxyURL(u)
		}
		return &http.Client{
			Transport: transport,
			Timeout:   8 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				// 只信任平台自身 API 的重定向，避免把 token 发送到其他主机。
				if len(via) >= 5 || req.URL.Scheme != "https" || req.URL.Host != via[0].URL.Host {
					return http.ErrUseLastResponse
				}
				return nil
			},
		}
	}
	return &identityResolver{
		direct: newClient(""), proxy: newClient(cfg.Direct.Proxy), tokens: cfg.IdentityTokens,
		cache: make(map[string]identityEntry),
	}
}

func (r *identityResolver) resolve(ctx context.Context, repo repository) repoIdentity {
	if repo.apiPath == "" {
		return repoIdentity{canonical: repo}
	}
	var endpoint string
	client := r.direct
	switch repo.host {
	case "github.com":
		endpoint, client = "https://api.github.com/repos/", r.proxy
	case "gitee.com":
		endpoint = "https://gitee.com/api/v5/repos/"
	case "gitcode.com":
		endpoint = "https://api.gitcode.com/api/v5/repos/"
	default:
		return repoIdentity{canonical: repo}
	}
	now := time.Now()
	r.mu.Lock()
	entry, ok := r.cache[repo.key]
	r.mu.Unlock()
	if ok && now.Before(entry.expires) {
		return entry.identity
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+repo.apiPath, nil)
	if err != nil {
		return repoIdentity{canonical: repo}
	}
	req.Header.Set("Accept", "application/json")
	if token := r.tokens[repo.host]; token != "" {
		if repo.host == "github.com" {
			req.Header.Set("Authorization", "Bearer "+token)
		} else {
			query := req.URL.Query()
			query.Set("access_token", token)
			req.URL.RawQuery = query.Encode()
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return repoIdentity{canonical: repo}
	}
	defer resp.Body.Close()
	var payload struct {
		ID       int64  `json:"id"`
		FullName string `json:"full_name"`
	}
	if resp.StatusCode != http.StatusOK ||
		json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload) != nil || payload.ID <= 0 {
		// 网络、权限或限额错误不更新身份，避免把故障误判为迁移。
		return repoIdentity{canonical: repo}
	}
	canonical, err := parseRepository("https://" + repo.host + "/" + payload.FullName)
	if err != nil || canonical.apiPath == "" {
		return repoIdentity{canonical: repo}
	}
	identity := repoIdentity{key: fmt.Sprintf("%s:%d", repo.host, payload.ID), canonical: canonical}
	r.mu.Lock()
	// 公共 API 有速率限制，短期复用成功结果，同时限制缓存占用。
	if len(r.cache) >= 4096 {
		clear(r.cache)
	}
	entry = identityEntry{identity: identity, expires: time.Now().Add(5 * time.Minute)}
	r.cache[repo.key], r.cache[canonical.key] = entry, entry
	r.mu.Unlock()
	return identity
}
