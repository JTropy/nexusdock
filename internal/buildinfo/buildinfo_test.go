package buildinfo

import "testing"

// go test 不经过 ldflags 注入，这里锁定缺省值契约：
// 任何来源的构建都必须保证三个字段非空，避免系统状态输出空版本。
func TestBuildInfoKeepsDefaultsWithoutInjection(t *testing.T) {
	if Version != "dev" {
		t.Fatalf("Version = %q, want default %q", Version, "dev")
	}
	if Revision != "unknown" {
		t.Fatalf("Revision = %q, want default %q", Revision, "unknown")
	}
	if Source != "unknown" {
		t.Fatalf("Source = %q, want default %q", Source, "unknown")
	}
}
