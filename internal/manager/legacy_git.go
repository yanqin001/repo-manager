package manager

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

func legacyGitVersion(output string) (bool, error) {
	fields := strings.Fields(output)
	if len(fields) < 3 || fields[0] != "git" || fields[1] != "version" {
		return false, fmt.Errorf("无法识别远端 Git 版本")
	}
	majorText, rest, _ := strings.Cut(fields[2], ".")
	minorText, _, _ := strings.Cut(rest, ".")
	major, majorErr := strconv.Atoi(majorText)
	minor, minorErr := strconv.Atoi(minorText)
	if majorErr != nil || minorErr != nil {
		return false, fmt.Errorf("无法识别远端 Git 版本")
	}
	return major < 2 || (major == 2 && minor < 29), nil
}

func (r relay) discoverLegacy(ctx context.Context, upstream string) (snapshot, error) {
	// 老 Git 没有 ls-remote --symref。直接读取 smart HTTP 通告，保留服务端给出的默认分支。
	data, err := r.command(ctx, "curl", "-q", "--noproxy", "*", "-fsSL", "--max-redirs", "5",
		"--connect-timeout", "10", "--max-time", "90", "--proto", "=http,https", "--proto-redir", "=http,https",
		strings.TrimRight(upstream, "/")+"/info/refs?service=git-upload-pack")
	if err != nil {
		return snapshot{}, fmt.Errorf("查询旧版 Git 的上游引用：%w", err)
	}
	return parseAdvertisement(data)
}

func parseAdvertisement(data string) (snapshot, error) {
	s := snapshot{refs: make(map[string]string)}
	// pkt-line 的四位十六进制长度包含自身，0000 是服务声明和引用列表的边界。
	service, ended := false, false
	head := ""
	for len(data) >= 4 {
		size, err := strconv.ParseUint(data[:4], 16, 16)
		if err != nil || size > uint64(len(data)) || (size != 0 && size < 4) {
			return s, fmt.Errorf("上游 Git 引用通告的包长度无效")
		}
		if size == 0 {
			data, ended = data[4:], true
			continue
		}
		line := strings.TrimSuffix(data[4:size], "\n")
		data, ended = data[size:], false
		if line == "# service=git-upload-pack" {
			service = true
			continue
		}
		refLine, caps, _ := strings.Cut(line, "\x00")
		oid, ref, ok := strings.Cut(refLine, " ")
		if !ok {
			continue
		}
		if ref == "HEAD" {
			head = oid
			for cap := range strings.FieldsSeq(caps) {
				if branch, ok := strings.CutPrefix(cap, "symref=HEAD:"+gitHeads); ok {
					s.branch = branch
				}
			}
			continue
		}
		if err := addAdvertisedRef(&s, ref, oid); err != nil {
			return s, err
		}
	}
	if len(data) != 0 || !service || !ended {
		return s, fmt.Errorf("上游未返回完整的 smart HTTP Git 引用通告")
	}
	return finishDiscovery(s, head)
}
