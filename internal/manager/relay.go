package manager

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

type relay struct {
	cfg      Config
	remote   inputRunner
	download commandRunner
	legacy   bool // Git 2.29 之前的远端使用 SHA-1 和旧版兼容命令。
}

func (r relay) command(ctx context.Context, program string, args ...string) (string, error) {
	return r.remote(ctx, "", program, args...)
}

func (r relay) git(ctx context.Context, dir string, args ...string) (string, error) {
	return remoteGit(r.remote, ctx, dir, "", args...)
}

func (r relay) synchronize(ctx context.Context, repo repository, rec repoRecord, rel, id string, local localRepo) (snapshot, error) {
	if r.remote == nil {
		run, close, err := openSSHRunner(r.cfg.Remote.Host)
		if err != nil {
			return snapshot{}, fmt.Errorf("准备 SSH 连接：%w", err)
		}
		defer close()
		r.remote = run
	}
	version, err := r.git(ctx, "", "--version")
	if err != nil {
		return snapshot{}, fmt.Errorf("查询远端 Git 版本：%w", err)
	}
	r.legacy, err = legacyGitVersion(version)
	if err != nil {
		return snapshot{}, err
	}
	var advertised snapshot
	if r.legacy {
		advertised, err = r.discoverLegacy(ctx, repo.cloneURL)
	} else {
		advertised, err = discover(ctx, r.git, repo.cloneURL)
	}
	if err != nil {
		return snapshot{}, err
	}
	if r.legacy && advertised.oidSize == 64 {
		return snapshot{}, fmt.Errorf("远端 Git 需要升级到 2.29 或更高版本才能同步 SHA-256 仓库")
	}
	cache, err := r.prepareRemote(ctx, repo, rec, rel, advertised.oidSize)
	if err != nil {
		return snapshot{}, fmt.Errorf("准备远端 Git 目录：%w", err)
	}
	s, err := fetchSnapshot(ctx, r.git, cache, repo.cloneURL, advertised.branch)
	if err != nil {
		return s, err
	}
	if local.matches(ctx, repo, s) {
		return s, nil
	}
	stage, cleanup, err := local.newStage(ctx, s.oidSize)
	if err != nil {
		return s, err
	}
	defer cleanup()

	if err := r.populate(ctx, repo, cache, stage, id, local.current, s); err != nil {
		return s, err
	}
	if err := publishGit(ctx, stage, repo, s); err != nil {
		return s, fmt.Errorf("准备雷速同步的工作树：%w", err)
	}
	return s, local.install(ctx, stage)
}

func (r relay) prepareRemote(ctx context.Context, repo repository, rec repoRecord, rel string, oidSize int) (string, error) {
	dir := path.Join(r.cfg.Remote.RepoPath, rel)
	if _, err := r.command(ctx, "mkdir", "-p", "--", path.Dir(dir)); err != nil {
		return "", err
	}
	format := "sha1"
	if oidSize == 64 {
		format = "sha256"
	}
	suffix := "__" + shortHash(repo.key+rec.Identity+format)
	for _, candidate := range []string{dir, dir + suffix} {
		// 远端可能已有旧工作树，只有 origin 已知且对象格式匹配时才复用。
		origin, originErr := gitOrigin(ctx, r.git, candidate)
		if originErr == nil && slices.Contains(rec.Origins, origin.key) {
			matchesFormat := r.legacy
			if !r.legacy {
				actual, err := r.git(ctx, candidate, "rev-parse", "--show-object-format")
				matchesFormat = err == nil && strings.TrimSpace(actual) == format
			}
			if matchesFormat {
				if _, err := r.git(ctx, candidate, "config", "--local", "--replace-all", "remote.origin.url", repo.cloneURL); err != nil {
					return "", err
				}
				return candidate, nil
			}
		}
		present, err := r.command(ctx, "sh", "-c", `if [ -e "$1" ] || [ -L "$1" ]; then printf present; fi`, "check-path", candidate)
		if err != nil {
			return "", err
		}
		if present != "" {
			continue
		}
		args := []string{"init", "--bare", "--quiet"}
		if !r.legacy {
			args = append(args, "--object-format="+format)
		}
		if _, err := r.git(ctx, "", append(args, "--", candidate)...); err != nil {
			return "", err
		}
		if _, err := r.git(ctx, candidate, "config", "--local", "remote.origin.url", repo.cloneURL); err != nil {
			return "", err
		}
		return candidate, nil
	}
	return "", fmt.Errorf("远端缓存目录与其他仓库冲突")
}

func (r relay) populate(ctx context.Context, repo repository, cache, stage, id string, previous, target snapshot) error {
	if len(target.refs) == 0 {
		return nil
	}
	localCheck := func(oids []string) (map[string]string, error) {
		return checkObjects(oids, func(input string) (string, error) {
			return gitInput(ctx, stage, input, "cat-file", "--batch-check")
		})
	}
	targetIDs := snapshotIDs(target)
	if objects, err := localCheck(targetIDs); err == nil && len(objects) == len(targetIDs) {
		return nil
	}
	bases := snapshotIDs(previous)
	if len(bases) > 0 {
		// 只排除双方都实际拥有的提交。标签可能指向非提交对象，不适合作为 bundle 前置提交。
		localObjects, localErr := localCheck(bases)
		remoteObjects, remoteErr := checkObjects(bases, func(input string) (string, error) {
			return remoteGit(r.remote, ctx, cache, input, "cat-file", "--batch-check")
		})
		if localErr == nil && remoteErr == nil {
			bases = slices.DeleteFunc(bases, func(oid string) bool {
				return localObjects[oid] != "commit" || remoteObjects[oid] != "commit"
			})
		} else {
			bases = nil
		}
	}
	if len(bases) > 0 {
		verifyFailure, err := r.transfer(ctx, repo, cache, stage, id, target, bases)
		if err == nil || !verifyFailure || ctx.Err() != nil {
			return err
		}
		// 前置提交、包内容或打包失败时重试全量包；传输错误直接保留原故障。
	}
	_, err := r.transfer(ctx, repo, cache, stage, id, target, nil)
	return err
}

func snapshotIDs(s snapshot) []string {
	out := make([]string, 0, len(s.refs))
	for _, oid := range s.refs {
		out = append(out, oid)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func checkObjects(oids []string, check func(string) (string, error)) (map[string]string, error) {
	out, err := check(strings.Join(oids, "\n") + "\n")
	if err != nil {
		return nil, err
	}
	objects := make(map[string]string)
	for line := range strings.SplitSeq(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && objectIDPattern.MatchString(fields[0]) {
			objects[fields[0]] = fields[1]
		}
	}
	return objects, nil
}

func (r relay) transfer(ctx context.Context, repo repository, cache, stage, id string, target snapshot, bases []string) (bool, error) {
	kind := "full"
	if len(bases) > 0 {
		kind = "delta"
	}
	// 域名隔离不同平台，仓库哈希和任务 ID 隔离展平重名及并发传输。
	filename := repo.name + "__" + shortHash(repo.key) + "__" + id + "_" + kind + ".bundle"
	remoteDir := path.Join(r.cfg.Remote.RepoPath, ".repo-manager-transfers", id+"-"+kind)
	if _, err := r.command(ctx, "mkdir", "-p", "--", path.Dir(remoteDir)); err != nil {
		return false, err
	}
	if _, err := r.command(ctx, "mkdir", "--", remoteDir); err != nil {
		return false, err
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, _ = r.command(cleanupCtx, "rm", "-rf", "--", remoteDir)
	}()
	remoteBundle := path.Join(remoteDir, filename)
	packStarted := time.Now()
	if err := r.createBundle(ctx, cache, remoteBundle, target, bases); err != nil {
		return true, fmt.Errorf("生成 %s bundle：%w", kind, err)
	}
	packDuration := time.Since(packStarted)
	checksum, err := r.command(ctx, "sha256sum", "--", remoteBundle)
	if err != nil {
		return false, fmt.Errorf("获取远端 bundle 校验值：%w", err)
	}
	fields := strings.Fields(checksum)
	if len(fields) == 0 || len(fields[0]) != 64 || !objectIDPattern.MatchString(fields[0]) {
		return false, fmt.Errorf("远端 bundle 校验值无效")
	}
	serverDir := path.Join(r.cfg.Rayfile.ServerPath, repo.host)
	serverBundle := path.Join(serverDir, filename)
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// 文件名由当前任务独占，也尝试清理中断的上传留下的部分文件。
		if _, err := r.download(cleanupCtx, r.cfg.Rayfile.LocalCmdPath, r.rayArgs("remove", serverBundle, "")...); err != nil {
			slog.Warn("雷速任务文件清理失败", "task", id, "bundle", kind, "error", redactError(err, r.cfg))
		}
	}()
	uploadStarted := time.Now()
	if _, err := r.command(ctx, r.cfg.Rayfile.RemoteCmdPath, r.rayArgs("upload", remoteBundle, serverDir)...); err != nil {
		return false, fmt.Errorf("雷速上传 %s bundle：%w", kind, err)
	}
	uploadDuration := time.Since(uploadStarted)
	downloadDir, err := os.MkdirTemp(filepath.Dir(stage), "bundle-")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(downloadDir)
	downloadStarted := time.Now()
	if _, err := r.download(ctx, r.cfg.Rayfile.LocalCmdPath, r.rayArgs("download", serverBundle, downloadDir)...); err != nil {
		return false, fmt.Errorf("雷速下载 %s bundle：%w", kind, err)
	}
	downloadDuration := time.Since(downloadStarted)
	localBundle := filepath.Join(downloadDir, filename)
	size, err := verifyChecksum(ctx, localBundle, fields[0])
	if err != nil {
		return true, fmt.Errorf("校验 %s bundle：%w", kind, err)
	}
	git := localGit("")
	if _, err := git(ctx, stage, "bundle", "verify", localBundle); err != nil {
		return true, fmt.Errorf("校验 %s bundle 的前置对象：%w", kind, err)
	}
	if _, err := git(ctx, stage, "bundle", "unbundle", localBundle); err != nil {
		return true, fmt.Errorf("导入 %s bundle：%w", kind, err)
	}
	slog.Info("雷速传输完成", "task", id, "bundle", kind, "bytes", size,
		"pack_duration", packDuration, "upload_duration", uploadDuration, "download_duration", downloadDuration)
	return false, nil
}

func (r relay) createBundle(ctx context.Context, cache, file string, target snapshot, bases []string) error {
	var input string
	args := []string{"bundle", "create", file}
	if r.legacy {
		// Git 1.8 的 bundle 不读取 --stdin，用两个 ref glob 覆盖全部引用。
		// 限制排除项只会增加重复传输，确保传给 SSH 的命令不会超过单个参数的长度。
		args = append(args, "--glob="+syncedHeads+"*", "--glob="+syncedTags+"*")
		for _, oid := range bases[:min(len(bases), 1024)] {
			args = append(args, "^"+oid)
		}
	} else {
		revisions := make([]string, 0, len(target.refs)+len(bases))
		for ref := range target.refs {
			revisions = append(revisions, stagedRef(ref))
		}
		slices.Sort(revisions)
		for _, oid := range bases {
			revisions = append(revisions, "^"+oid)
		}
		input = strings.Join(revisions, "\n") + "\n"
		args = append(args, "--stdin")
	}
	_, err := remoteGit(r.remote, ctx, cache, input, args...)
	return err
}

func (r relay) rayArgs(operation, source, destination string) []string {
	cfg := r.cfg.Rayfile
	args := []string{"-a", cfg.Host, "-P", strconv.Itoa(cfg.Port), "-u", cfg.User, "-w", cfg.Password,
		"-retry", "100", "-min", "100", "-o", operation, "-s", source}
	if destination != "" {
		args = append(args, "-d", destination)
	}
	return args
}

func verifyChecksum(ctx context.Context, file, expected string) (int64, error) {
	info, err := os.Lstat(file)
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() {
		return 0, fmt.Errorf("雷速下载结果不是普通文件")
	}
	f, err := os.Open(file)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, deadlineReader{ctx: ctx, input: f})
	if err != nil {
		return 0, err
	}
	if hex.EncodeToString(h.Sum(nil)) != expected {
		return 0, fmt.Errorf("SHA-256 校验不匹配")
	}
	return n, nil
}

// deadlineReader 让大 bundle 的校验也响应任务超时和服务退出。
type deadlineReader struct {
	ctx   context.Context
	input io.Reader
}

// Read 读取下一段校验数据，并在任务取消或超时时返回 context 错误。
func (r deadlineReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.input.Read(p)
}
