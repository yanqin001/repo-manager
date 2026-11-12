package manager

import (
	"fmt"
	"strings"
	"testing"
)

// TestLegacyAdvertisement 验证旧 Git 路径能识别默认分支和标签，并拒绝截断通告。
func TestLegacyAdvertisement(t *testing.T) {
	packet := func(body string) string {
		return fmt.Sprintf("%04x%s", len(body)+4, body)
	}
	head := strings.Repeat("a", 40)
	tag := strings.Repeat("b", 40)
	data := packet("# service=git-upload-pack\n") + "0000" +
		packet(head+" HEAD\x00symref=HEAD:refs/heads/z-default object-format=sha1\n") +
		packet(head+" refs/heads/a-other\n") + packet(head+" refs/heads/z-default\n") +
		packet(tag+" refs/tags/v1\n") + packet(head+" refs/tags/v1^{}\n") +
		packet(head+" refs/pull/1/head\n") + "0000"
	got, err := parseAdvertisement(data)
	if err != nil || got.branch != "z-default" || got.commit() != head ||
		got.refs["refs/tags/v1"] != tag || len(got.refs) != 3 || got.oidSize != 40 {
		t.Fatalf("引用通告解析结果 %+v，错误：%v", got, err)
	}
	if _, err := parseAdvertisement(data[:len(data)-5]); err == nil {
		t.Fatal("接受了截断的引用通告")
	}
}
