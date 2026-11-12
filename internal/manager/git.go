package manager

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	// syncedHeads 和 syncedTags 用于下载阶段，只有通过校验后才写正式引用。
	syncedHeads = "refs/repo-manager/heads/"
	syncedTags  = "refs/repo-manager/tags/"
	originHeads = "refs/remotes/origin/"
	gitHeads    = "refs/heads/"
	gitTags     = "refs/tags/"
)

var objectIDPattern = regexp.MustCompile(`^(?:[a-f0-9]{40}|[a-f0-9]{64})$`) // Git 的 SHA-1 或 SHA-256 对象 ID。

type snapshot struct {
	refs    map[string]string // 上游 refs/heads 和 refs/tags 到对象 ID。
	branch  string            // 上游 HEAD 指向的分支短名。
	oidSize int               // 对象 ID 长度，空仓库可为零。
}

func (s snapshot) commit() string { return s.refs[gitHeads+s.branch] }

func (s *snapshot) add(ref, oid string) error {
	if !objectIDPattern.MatchString(oid) || (s.oidSize != 0 && len(oid) != s.oidSize) ||
		strings.ContainsAny(ref, " \t\r\n\x00") {
		return fmt.Errorf("Git 返回无效引用")
	}
	s.oidSize = len(oid)
	s.refs[ref] = oid
	return nil
}

func (s snapshot) validate() error {
	if _, reserved := s.refs[gitHeads+"HEAD"]; reserved {
		return fmt.Errorf("上游分支 HEAD 与 origin/HEAD 冲突")
	}
	if s.branch != "" && s.commit() == "" {
		return fmt.Errorf("上游默认分支 %s 在同步期间发生变化，请重试", s.branch)
	}
	return nil
}

func discover(ctx context.Context, git gitRunner, upstream string) (snapshot, error) {
	out, err := git(ctx, "", "ls-remote", "--symref", "--", upstream, "HEAD", "refs/heads/*", "refs/tags/*")
	if err != nil {
		return snapshot{}, fmt.Errorf("查询上游引用：%w", err)
	}
	s := snapshot{refs: make(map[string]string)}
	head := ""
	for line := range strings.SplitSeq(out, "\n") {
		oid, ref, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		if ref == "HEAD" {
			if branch, ok := strings.CutPrefix(oid, "ref: "+gitHeads); ok {
				s.branch = branch
			} else {
				head = oid
			}
		} else if err := addAdvertisedRef(&s, ref, oid); err != nil {
			return s, err
		}
	}
	return finishDiscovery(s, head)
}

func addAdvertisedRef(s *snapshot, ref, oid string) error {
	if strings.HasPrefix(ref, gitHeads) || (strings.HasPrefix(ref, gitTags) && !strings.HasSuffix(ref, "^{}")) {
		return s.add(ref, oid)
	}
	return nil
}

func finishDiscovery(s snapshot, head string) (snapshot, error) {
	if s.branch == "" {
		// 上游未通告 HEAD 指向时，优先从同一提交上的分支选择确定的入口。
		first := ""
		for ref, oid := range s.refs {
			name, ok := strings.CutPrefix(ref, gitHeads)
			if !ok {
				continue
			}
			if first == "" || name < first {
				first = name
			}
			if oid == head && (s.branch == "" || name < s.branch) {
				s.branch = name
			}
		}
		if s.branch == "" {
			s.branch = first
		}
	}
	return s, s.validate()
}

func fetchSnapshot(ctx context.Context, git gitRunner, dir, upstream, branch string) (snapshot, error) {
	// fetch 只写暂存引用。先完成 prune，Git 才能用 foo/bar 替换原有的 foo。
	// 仓库 URL 已校验为 HTTP(S)，直接作为位置参数兼容旧版 Git。
	_, err := git(ctx, dir, "fetch", "--prune", "--no-tags", "--no-recurse-submodules",
		upstream, "+"+gitHeads+"*:"+syncedHeads+"*", "+"+gitTags+"*:"+syncedTags+"*")
	if err != nil {
		return snapshot{}, fmt.Errorf("拉取上游分支和标签：%w", err)
	}
	return readSnapshot(ctx, git, dir, branch, syncedHeads, syncedTags)
}

func readSnapshot(ctx context.Context, git gitRunner, dir, branch, heads, tags string) (snapshot, error) {
	out, err := git(ctx, dir, "for-each-ref", "--format=%(objectname)%09%(refname)%09%(symref)", heads, tags)
	s := snapshot{refs: make(map[string]string), branch: branch}
	if err != nil {
		return s, err
	}
	for line := range strings.SplitSeq(out, "\n") {
		oid, info, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		ref, symref, _ := strings.Cut(info, "\t")
		if symref != "" {
			continue
		}
		prefix := gitHeads
		suffix, isHead := strings.CutPrefix(ref, heads)
		if !isHead {
			prefix = gitTags
			suffix = strings.TrimPrefix(ref, tags)
		}
		if err := s.add(prefix+suffix, oid); err != nil {
			return s, err
		}
	}
	return s, s.validate()
}

func gitOrigin(ctx context.Context, git gitRunner, dir string) (repository, error) {
	origin, err := git(ctx, dir, "config", "--local", "--get", "remote.origin.url")
	if err != nil {
		return repository{}, err
	}
	return parseRepository(strings.TrimSpace(origin))
}

func localOrigin(ctx context.Context, dir string) (repository, error) {
	if err := realDirectory(filepath.Join(dir, ".git")); err != nil {
		return repository{}, err
	}
	return gitOrigin(ctx, localGit(""), dir)
}

type localRepo struct {
	path, target string
	info         os.FileInfo
	clean        bool
	current      snapshot
}

func inspectLocal(ctx context.Context, dir string, rec repoRecord) (localRepo, error) {
	l := localRepo{path: dir, target: dir}
	if err := realDirectory(filepath.Dir(dir)); err != nil {
		return l, err
	}
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return l, err
	}
	linked := info.Mode()&os.ModeSymlink != 0
	if linked {
		l.target, err = filepath.EvalSymlinks(dir)
		if err != nil {
			return l, fmt.Errorf("解析已有仓库软链接：%w", err)
		}
		if err := realDirectory(l.target); err != nil {
			return l, err
		}
		relative, err := filepath.Rel(l.target, dir)
		if err != nil || relative == "." || (!strings.HasPrefix(relative, ".."+string(os.PathSeparator)) && relative != "..") {
			return l, fmt.Errorf("软链接目标包含正式仓库路径")
		}
		info, err = os.Stat(l.target)
		if err != nil {
			return l, err
		}
	}
	l.info = info
	if !info.IsDir() {
		return l, nil
	}
	origin, err := localOrigin(ctx, l.target)
	if err != nil || !slices.Contains(rec.Origins, origin.key) {
		if linked {
			// 软链接可能指向 /source 之外，只有确认归属后才能替换其目标。
			return l, fmt.Errorf("软链接目标的 Git origin 与受管仓库不符")
		}
		return l, nil
	}

	git := localGit("")
	shallow, err := git(ctx, l.target, "rev-parse", "--is-shallow-repository")
	if err != nil || strings.TrimSpace(shallow) != "false" {
		return l, nil
	}
	promisor, _ := git(ctx, l.target, "config", "--local", "--get-regexp", `^remote\..*\.promisor$`)
	if promisor != "" {
		return l, nil
	}
	format, err := git(ctx, l.target, "rev-parse", "--show-object-format")
	if err != nil {
		return l, nil
	}
	status, err := git(ctx, l.target, "status", "--porcelain", "--untracked-files=normal", "--ignore-submodules=none")
	if err != nil || status != "" {
		return l, nil
	}
	branch, err := git(ctx, l.target, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		return l, nil
	}
	branch = strings.TrimPrefix(strings.TrimSpace(branch), gitHeads)
	head, err := git(ctx, l.target, "rev-parse", "--verify", "HEAD")
	if err != nil {
		// 空仓库的 HEAD 尚未指向提交，仍可检查引用以跳过重复发布。
		branch = ""
	}
	l.current, err = readSnapshot(ctx, git, l.target, branch, originHeads, gitTags)
	if err != nil || (branch != "" && l.current.commit() != strings.TrimSpace(head)) {
		return l, nil
	}
	if l.current.oidSize == 0 {
		l.current.oidSize = 40
		if strings.TrimSpace(format) == "sha256" {
			l.current.oidSize = 64
		}
	}
	l.clean = true
	return l, nil
}

func (l localRepo) newStage(ctx context.Context, oidSize int) (dir string, cleanup func(), err error) {
	parent, err := os.MkdirTemp(filepath.Dir(l.target), ".repo-manager-stage-")
	if err != nil {
		return "", nil, err
	}
	cleanup = func() { _ = os.RemoveAll(parent) }
	dir = filepath.Join(parent, "repo")
	git := localGit("")
	alternates := filepath.Join(l.target, ".git", "objects", "info", "alternates")
	_, altErr := os.Lstat(alternates)
	// 本地 clone 复用不可变 Git 对象的硬链接，暂存区不依赖旧目录的存续。
	if l.clean && (oidSize == 0 || l.current.oidSize == 0 || oidSize == l.current.oidSize) &&
		errors.Is(altErr, os.ErrNotExist) {
		if _, cloneErr := git(ctx, "", "-c", "protocol.file.allow=always", "clone",
			"--local", "--no-checkout", "--no-tags", "--", l.target, dir); cloneErr == nil {
			var seed strings.Builder
			for ref, oid := range l.current.refs {
				fmt.Fprintf(&seed, "update %s %s\n", stagedRef(ref), oid)
			}
			// 保留旧分支和标签作为协商起点，避免上游重传已有的其他分支对象。
			if _, seedErr := gitInput(ctx, dir, seed.String(), "update-ref", "--stdin"); seedErr == nil {
				return dir, cleanup, nil
			}
		}
		if err := os.RemoveAll(dir); err != nil {
			cleanup()
			return "", nil, err
		}
	}
	format := "sha1"
	if oidSize == 64 {
		format = "sha256"
	}
	if _, err := git(ctx, "", "init", "--quiet", "--initial-branch=main", "--object-format="+format, "--", dir); err != nil {
		cleanup()
		return "", nil, err
	}
	return dir, cleanup, nil
}

func stagedRef(ref string) string {
	if branch, ok := strings.CutPrefix(ref, gitHeads); ok {
		return syncedHeads + branch
	}
	return syncedTags + strings.TrimPrefix(ref, gitTags)
}

func publishGit(ctx context.Context, dir string, repo repository, s snapshot) error {
	git := localGit("")
	// 在暂存仓库先删除旧引用，再建新引用，兼容 foo 被 foo/bar 替换。
	_, _ = git(ctx, dir, "symbolic-ref", "--delete", originHeads+"HEAD")
	current, err := git(ctx, dir, "for-each-ref", "--format=%(refname)", originHeads, gitHeads, gitTags, syncedHeads, syncedTags)
	if err != nil {
		return err
	}
	var deletes strings.Builder
	for ref := range strings.FieldsSeq(current) {
		fmt.Fprintf(&deletes, "delete %s\n", ref)
	}
	if _, err := gitInput(ctx, dir, deletes.String(), "update-ref", "--stdin"); err != nil {
		return err
	}
	var updates strings.Builder
	keys := make([]string, 0, len(s.refs))
	for ref := range s.refs {
		keys = append(keys, ref)
	}
	sort.Strings(keys)
	for _, ref := range keys {
		dest := ref
		if name, ok := strings.CutPrefix(ref, gitHeads); ok {
			dest = originHeads + name
		}
		fmt.Fprintf(&updates, "update %s %s\n", dest, s.refs[ref])
	}
	if _, err := gitInput(ctx, dir, updates.String(), "update-ref", "--stdin"); err != nil {
		return fmt.Errorf("设置本地分支和标签：%w", err)
	}
	for _, config := range [][2]string{
		{"remote.origin.url", repo.cloneURL},
		{"remote.origin.fetch", "+" + gitHeads + "*:" + originHeads + "*"},
	} {
		if _, err := git(ctx, dir, "config", "--local", "--replace-all", config[0], config[1]); err != nil {
			return err
		}
	}
	if s.branch == "" {
		return nil
	}
	if _, err := git(ctx, dir, "checkout", "--force", "-B", s.branch, s.commit(), "--"); err != nil {
		return fmt.Errorf("检出默认分支：%w", err)
	}
	if _, err := git(ctx, dir, "branch", "--set-upstream-to="+originHeads+s.branch, "--", s.branch); err != nil {
		return err
	}
	if _, err := git(ctx, dir, "symbolic-ref", originHeads+"HEAD", originHeads+s.branch); err != nil {
		return err
	}
	return nil
}

func (l localRepo) install(ctx context.Context, stage string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if l.path != l.target {
		target, err := filepath.EvalSymlinks(l.path)
		if err != nil || target != l.target {
			return fmt.Errorf("仓库软链接在同步期间发生变化")
		}
	}
	info, err := os.Lstat(l.target)
	if l.info == nil && errors.Is(err, os.ErrNotExist) {
		return os.Rename(stage, l.target)
	}
	if err != nil || l.info == nil || !os.SameFile(l.info, info) {
		return fmt.Errorf("仓库路径在同步期间发生变化")
	}
	// 交换完整目录，调用方通过原路径访问时始终有一个完整的 Git 副本。
	if err := unix.Renameat2(unix.AT_FDCWD, stage, unix.AT_FDCWD, l.target, unix.RENAME_EXCHANGE); err != nil {
		return fmt.Errorf("原子替换仓库目录 %s：%w", l.target, err)
	}
	return nil
}

func (l localRepo) matches(ctx context.Context, repo repository, s snapshot) bool {
	if !l.clean || l.current.branch != s.branch || len(l.current.refs) != len(s.refs) {
		return false
	}
	for ref, oid := range s.refs {
		if l.current.refs[ref] != oid {
			return false
		}
	}
	git := localGit("")
	origin, err := git(ctx, l.target, "config", "--local", "--get", "remote.origin.url")
	if err != nil || strings.TrimSpace(origin) != repo.cloneURL {
		return false
	}
	ref, _ := git(ctx, l.target, "symbolic-ref", "--quiet", originHeads+"HEAD")
	return (s.branch == "" && ref == "") || strings.TrimSpace(ref) == originHeads+s.branch
}
