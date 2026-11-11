package manager

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

// stateDirName 在仓库根目录中保留给持久化状态和服务进程锁。
const stateDirName = ".repo-manager"

// repoRecord 保留迁移前的 origin，供接管旧目录和校验软链接目标使用。
type repoRecord struct {
	Identity string   `json:"identity,omitempty"`
	URL      string   `json:"url"`
	Origins  []string `json:"origins"`
}

type registryData struct {
	Repos   map[string]repoRecord `json:"repositories"` // 域名和目录名组成的相对路径。
	Aliases map[string]string     `json:"aliases"`      // URL 键到当前仓库相对路径。
}

type registry struct {
	root, file string
	lease      *os.File
	mu         sync.Mutex
	data       registryData
	gates      map[string]chan struct{}
}

func openRegistry(root string) (*registry, error) {
	if err := os.MkdirAll(root, 0755); err != nil {
		return nil, err
	}
	base := filepath.Join(root, stateDirName)
	if err := os.Mkdir(base, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	if err := realDirectory(base); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(base, "service.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	// 共享 /source 时只能有一个写入服务，避免跨进程覆盖身份记录和正式目录。
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("无法锁定仓库根目录 %s：%w", root, err)
	}
	r := &registry{
		root: root, file: filepath.Join(base, "repositories.json"), lease: f,
		data:  registryData{Repos: make(map[string]repoRecord), Aliases: make(map[string]string)},
		gates: make(map[string]chan struct{}),
	}
	raw, err := os.ReadFile(r.file)
	if err == nil {
		err = json.Unmarshal(raw, &r.data)
		if err == nil {
			err = r.validate()
		}
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		f.Close()
		return nil, fmt.Errorf("读取仓库登记表：%w", err)
	}
	return r, nil
}

func (r *registry) validate() error {
	if r.data.Repos == nil || r.data.Aliases == nil {
		return fmt.Errorf("仓库登记表缺少必要字段")
	}
	for rel, rec := range r.data.Repos {
		domain, name, ok := strings.Cut(rel, "/")
		if !ok || !validDomain(domain) || !validSegment(name) {
			return fmt.Errorf("仓库登记表含无效路径")
		}
		if repo, err := parseRepository(rec.URL); err != nil || repo.host != domain {
			return fmt.Errorf("仓库登记表含无效地址")
		}
	}
	for _, rel := range r.data.Aliases {
		if _, ok := r.data.Repos[rel]; !ok {
			return fmt.Errorf("仓库登记表中的别名没有目标仓库")
		}
	}
	return nil
}

func (r *registry) reserve(ctx context.Context, request repository, identity repoIdentity) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	canonical := identity.canonical
	rel := r.find(request, identity)
	if rel == "" {
		var err error
		rel, err = r.allocate(ctx, request, canonical, identity.key)
		if err != nil {
			return "", err
		}
	}
	old, exists := r.data.Repos[rel]
	rec := old
	if identity.key != "" || !exists {
		rec.URL, rec.Identity = canonical.cloneURL, identity.key
	} else if resolved, err := parseRepository(rec.URL); err == nil {
		canonical = resolved
	}
	rec.Origins = slices.Clone(old.Origins)
	aliases := []string{request.key, canonical.key}
	for _, key := range aliases {
		if !slices.Contains(rec.Origins, key) {
			rec.Origins = append(rec.Origins, key)
		}
	}
	previous := make(map[string]string)
	for _, key := range aliases {
		previous[key] = r.data.Aliases[key]
	}
	if exists && old.URL == rec.URL && old.Identity == rec.Identity &&
		slices.Equal(old.Origins, rec.Origins) && previous[request.key] == rel && previous[canonical.key] == rel {
		return rel, nil
	}

	r.data.Repos[rel] = rec
	for _, key := range aliases {
		r.data.Aliases[key] = rel
	}
	if err := r.persist(); err != nil {
		if exists {
			r.data.Repos[rel] = old
		} else {
			delete(r.data.Repos, rel)
		}
		for key, target := range previous {
			if target == "" {
				delete(r.data.Aliases, key)
			} else {
				r.data.Aliases[key] = target
			}
		}
		return "", err
	}
	return rel, nil
}

func (r *registry) find(request repository, identity repoIdentity) string {
	if identity.key != "" {
		for rel, rec := range r.data.Repos {
			if rec.Identity == identity.key {
				return rel
			}
		}
	}
	for _, key := range []string{identity.canonical.key, request.key} {
		rel, exists := r.data.Aliases[key]
		if exists {
			rec := r.data.Repos[rel]
			if identity.key == "" || rec.Identity == "" || rec.Identity == identity.key {
				return rel
			}
		}
	}
	return ""
}

func (r *registry) allocate(ctx context.Context, request, canonical repository, id string) (string, error) {
	domain := filepath.Join(r.root, canonical.host)
	if err := os.Mkdir(domain, 0755); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	if err := realDirectory(domain); err != nil {
		return "", err
	}
	// 迁移后优先接管已有目录，同时用 origin 区分两个展平后重名的仓库。
	candidates := []string{canonical.name}
	if canonical.name != request.name {
		candidates = append(candidates, request.name)
	}
	for _, name := range candidates {
		rel := canonical.host + "/" + name
		if _, reserved := r.data.Repos[rel]; reserved {
			continue
		}
		local := filepath.Join(r.root, rel)
		origin, err := localOrigin(ctx, local)
		if err == nil && (origin.key == request.key || origin.key == canonical.key) {
			return rel, nil
		}
	}
	seed := canonical.key
	if id != "" {
		seed = id
	}
	for i := 0; i < 100; i++ {
		name := canonical.name
		if i > 0 {
			name += "__" + shortHash(fmt.Sprintf("%s:%d", seed, i))
		}
		rel := canonical.host + "/" + name
		if _, reserved := r.data.Repos[rel]; reserved {
			continue
		}
		if _, err := os.Lstat(filepath.Join(r.root, rel)); errors.Is(err, os.ErrNotExist) {
			return rel, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("无法为 %s 分配目录", canonical.url)
}

func (r *registry) acquire(ctx context.Context, rel string) (repoRecord, func(), error) {
	r.mu.Lock()
	gate := r.gates[rel]
	if gate == nil {
		gate = make(chan struct{}, 1)
		r.gates[rel] = gate
	}
	r.mu.Unlock()
	select {
	case <-ctx.Done():
		return repoRecord{}, nil, ctx.Err()
	case gate <- struct{}{}:
	}
	r.mu.Lock()
	rec := r.data.Repos[rel]
	r.mu.Unlock()
	return rec, func() { <-gate }, nil
}

func (r *registry) persist() error {
	f, err := os.CreateTemp(filepath.Dir(r.file), "repositories-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := json.NewEncoder(f).Encode(r.data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// 同一文件系统内发布完整登记表，重启后路径和别名仍能复用。
	return os.Rename(f.Name(), r.file)
}

func realDirectory(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s 必须是实际目录", dir)
	}
	return nil
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}
