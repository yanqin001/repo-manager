package manager

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"
)

type commandRunner func(context.Context, string, ...string) (string, error)
type inputRunner func(context.Context, string, string, ...string) (string, error)

func localCommand(ctx context.Context, program string, args ...string) (string, error) {
	return execute(ctx, "", program, args, nil, nil)
}

func execute(ctx context.Context, dir, program string, args, env []string, input *strings.Reader) (string, error) {
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.WaitDelay = 5 * time.Second
	if input != nil {
		cmd.Stdin = input
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(string(out))
		}
		if len(detail) > 4096 {
			detail = detail[len(detail)-4096:]
		}
		// 避免依赖包含完整参数的错误信息，代理和雷速命令可能带凭据。
		return "", fmt.Errorf("%s: %w %s", program, err, detail)
	}
	return string(out), nil
}

// gitEnvironment 屏蔽会改写 Git 目录、代理或凭据来源的进程配置。
func gitEnvironment() []string {
	var env []string
	for _, v := range os.Environ() {
		name, _, _ := strings.Cut(v, "=")
		if !strings.HasPrefix(name, "GIT_") && !strings.HasSuffix(strings.ToLower(name), "_proxy") {
			env = append(env, v)
		}
	}
	return append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C")
}

func gitArgs(proxy string, args ...string) []string {
	return append([]string{
		"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "protocol.allow=never",
		"-c", "protocol.https.allow=always", "-c", "protocol.http.allow=always",
		"-c", "http.proxy=" + proxy, "-c", "http.followRedirects=initial",
		"-c", "http.lowSpeedLimit=1", "-c", "http.lowSpeedTime=60",
	}, args...)
}

type gitRunner func(ctx context.Context, dir string, args ...string) (string, error)

func localGit(proxy string) gitRunner {
	return func(ctx context.Context, dir string, args ...string) (string, error) {
		return execute(ctx, dir, "git", gitArgs(proxy, args...), gitEnvironment(), nil)
	}
}

func gitInput(ctx context.Context, dir, input string, args ...string) (string, error) {
	return execute(ctx, dir, "git", gitArgs("", args...), gitEnvironment(), strings.NewReader(input))
}

func sshCommand(host string) inputRunner {
	return func(ctx context.Context, input, program string, args ...string) (string, error) {
		parts := append([]string{program}, args...)
		for i := range parts {
			parts[i] = shellQuote(parts[i])
		}
		// SSH 传入远端 shell 的命令只由逐项引用后的参数组成。
		return execute(ctx, "", "ssh", []string{"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes",
			"-o", "ConnectTimeout=15", "-T", "--", host, "exec " + strings.Join(parts, " ")}, nil, strings.NewReader(input))
	}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func remoteGit(remote inputRunner, ctx context.Context, dir, input string, args ...string) (string, error) {
	if dir != "" {
		args = append([]string{"-C", dir}, args...)
	}
	env := []string{"-u", "http_proxy", "-u", "https_proxy", "-u", "all_proxy", "-u", "HTTP_PROXY",
		"-u", "HTTPS_PROXY", "-u", "ALL_PROXY", "-u", "GIT_DIR", "-u", "GIT_WORK_TREE",
		"GIT_CONFIG_COUNT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C", "git"}
	return remote(ctx, input, "env", append(env, gitArgs("", args...)...)...)
}

func redactError(err error, cfg Config) string {
	message := err.Error()
	secrets := []string{cfg.Direct.Proxy, cfg.Rayfile.Password}
	if u, parseErr := url.Parse(cfg.Direct.Proxy); parseErr == nil && u.User != nil {
		if password, ok := u.User.Password(); ok {
			secrets = append(secrets, password, url.QueryEscape(password))
		}
	}
	for _, token := range cfg.IdentityTokens {
		secrets = append(secrets, token)
	}
	for _, secret := range secrets {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[已隐藏]")
		}
	}
	return message
}
