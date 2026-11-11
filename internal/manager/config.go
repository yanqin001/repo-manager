// Package manager 实现仓库身份识别、同步调度和本地目录管理。
package manager

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config 保存 repo-manager 的部署参数，由 LoadConfig 校验后交给 New。
type Config struct {
	// LocalRepoPath 是 SDK 调用方可访问的仓库根目录。
	LocalRepoPath string `yaml:"local_repo_path"`
	// ListenAddr 是 HTTP 服务监听地址，默认仅监听本机。
	ListenAddr string `yaml:"listen_addr"`
	// JobTimeout 是同步任务的最长执行时间，包含排队和回退。
	JobTimeout time.Duration `yaml:"job_timeout"`
	// Direct 限制本地 Git 操作的并发并设置国外仓库使用的代理。
	Direct struct {
		Proxy          string        `yaml:"proxy"`           // HTTP、HTTPS 或 SOCKS5 代理 URL。
		MaxConcurrency int           `yaml:"max_concurrency"` // 直连和代理任务共用此上限。
		ProxyTimeout   time.Duration `yaml:"proxy_timeout"`   // 代理尝试的最长时间，超时后切换到远端。
	} `yaml:"direct"`
	// Rayfile 配置上传、下载程序及服务端连接。
	Rayfile struct {
		LocalCmdPath   string `yaml:"local_cmd_path"`  // 本地下载程序的绝对路径。
		RemoteCmdPath  string `yaml:"remote_cmd_path"` // SSH 主机上上传程序的绝对路径。
		Host           string `yaml:"host"`            // 雷速服务端主机。
		Port           int    `yaml:"port"`            // 雷速服务端端口。
		User           string `yaml:"user"`            // 雷速服务账号。
		Password       string `yaml:"password"`        // 仅供雷速命令使用的密码。
		ServerPath     string `yaml:"server_path"`     // 服务端传输根目录，默认为 /data/code。
		MaxConcurrency int    `yaml:"max_concurrency"` // 整条 SSH 和雷速链路的并发上限。
	} `yaml:"rayfile"`
	// Remote 配置国外仓库回退时使用的 SSH 主机及 Git 缓存根目录。
	Remote struct {
		Host     string `yaml:"host"`      // SSH 目标，未指定用户时使用 root。
		RepoPath string `yaml:"repo_path"` // 远端 Git 缓存的绝对路径。
	} `yaml:"remote"`
	// IdentityTokens 按域名提供可选的平台 API token，用于提高限额或访问需要令牌的身份 API。
	IdentityTokens map[string]string `yaml:"identity_tokens"`
}

// LoadConfig 读取 filename 指定的 YAML，填充默认值，并拒绝不支持的字段或无效路径。
func LoadConfig(filename string) (Config, error) {
	var cfg Config
	f, err := os.Open(filename)
	if err != nil {
		return cfg, err
	}
	defer f.Close()
	decoder := yaml.NewDecoder(f)
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		// YAML 解析错误可能带有原始字段值，避免把配置里的凭据写入日志。
		return cfg, fmt.Errorf("无法解析配置 %s，请检查 YAML 格式和字段", filename)
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = "127.0.0.1:8080"
	}
	if cfg.JobTimeout == 0 {
		cfg.JobTimeout = 6 * time.Hour
	}
	if cfg.Direct.MaxConcurrency == 0 {
		cfg.Direct.MaxConcurrency = 2
	}
	if cfg.Direct.ProxyTimeout == 0 {
		cfg.Direct.ProxyTimeout = time.Hour
	}
	if cfg.Rayfile.MaxConcurrency == 0 {
		cfg.Rayfile.MaxConcurrency = 1
	}
	if cfg.Rayfile.ServerPath == "" {
		cfg.Rayfile.ServerPath = "/data/code"
	}
	if cfg.Remote.Host != "" && !strings.Contains(cfg.Remote.Host, "@") {
		cfg.Remote.Host = "root@" + cfg.Remote.Host
	}
	cfg.LocalRepoPath = filepath.Clean(cfg.LocalRepoPath)
	cfg.Remote.RepoPath = path.Clean(cfg.Remote.RepoPath)
	cfg.Rayfile.ServerPath = path.Clean(cfg.Rayfile.ServerPath)
	return cfg, cfg.validate()
}

func (c Config) validate() error {
	if !filepath.IsAbs(c.LocalRepoPath) || c.LocalRepoPath == "/" {
		return fmt.Errorf("local_repo_path 必须是非根绝对路径")
	}
	if _, _, err := net.SplitHostPort(c.ListenAddr); err != nil {
		return fmt.Errorf("listen_addr 必须是 host:port")
	}
	if c.JobTimeout <= 0 || c.Direct.ProxyTimeout <= 0 || c.Direct.MaxConcurrency <= 0 || c.Rayfile.MaxConcurrency <= 0 {
		return fmt.Errorf("并发限制和任务超时必须为正数")
	}
	if c.Direct.Proxy != "" {
		u, err := url.Parse(c.Direct.Proxy)
		if err != nil || u.Hostname() == "" ||
			(u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h") {
			return fmt.Errorf("direct.proxy 必须是 HTTP、HTTPS 或 SOCKS5 代理 URL")
		}
	}
	parts := strings.Split(c.Remote.Host, "@")
	if len(parts) != 2 || !validSegment(parts[0]) || !validDomain(parts[1]) ||
		!path.IsAbs(c.Remote.RepoPath) || c.Remote.RepoPath == "/" {
		return fmt.Errorf("remote.host 或 remote.repo_path 无效")
	}
	if !filepath.IsAbs(c.Rayfile.LocalCmdPath) || !path.IsAbs(c.Rayfile.RemoteCmdPath) ||
		!path.IsAbs(c.Rayfile.ServerPath) || c.Rayfile.ServerPath == "/" ||
		c.Rayfile.Host == "" || c.Rayfile.Port < 1 || c.Rayfile.Port > 65535 ||
		c.Rayfile.User == "" || c.Rayfile.Password == "" {
		return fmt.Errorf("rayfile 的命令路径、服务端路径或连接参数无效")
	}
	return nil
}
