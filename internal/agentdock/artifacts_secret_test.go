package agentdock

import (
	"bytes"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestArtifactSigningSecretIsAtomicAndSecure(t *testing.T) {
	dataDir := t.TempDir()
	const workers = 8
	results := make(chan string, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			service := NewArtifactService(dataDir)
			signature, err := service.Sign("node_secret", "artifact1", "report.txt", strings.Repeat("a", 64), 12345)
			if err != nil {
				errs <- err
				return
			}
			results <- signature
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	var expected string
	for signature := range results {
		if len(signature) != 64 {
			t.Fatalf("signature hex length = %d", len(signature))
		}
		if expected == "" {
			expected = signature
			continue
		}
		if signature != expected {
			t.Fatal("concurrent services observed different Artifact signing secrets")
		}
	}

	secretDir := filepath.Join(dataDir, "secrets")
	secretPath := filepath.Join(secretDir, "artifact-url-secret")
	dirInfo, err := os.Stat(secretDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("secret dir mode = %o, want 700", got)
	}
	fileInfo, err := os.Stat(secretPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := fileInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("secret file mode = %o, want 600", got)
	}
	raw, err := os.ReadFile(secretPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(raw, []byte("\n")) {
		t.Fatalf("secret file must end with newline: %q", raw)
	}
	decoded, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(decoded) != 32 {
		t.Fatalf("stored secret is not canonical hex: err=%v len=%d", err, len(decoded))
	}
	// 落盘密钥必须能复现服务给出的签名，保证跨进程签名一致。
	service := NewArtifactService(dataDir)
	signature, err := service.Sign("node_secret", "artifact1", "report.txt", strings.Repeat("a", 64), 12345)
	if err != nil {
		t.Fatal(err)
	}
	if signature != expected {
		t.Fatalf("recomputed signature = %s, want %s", signature, expected)
	}
}

func TestArtifactSigningSecretRejectsSymlink(t *testing.T) {
	dataDir := t.TempDir()
	secretDir := filepath.Join(dataDir, "secrets")
	if err := os.MkdirAll(secretDir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dataDir, "target")
	if err := os.WriteFile(target, []byte(strings.Repeat("a", 64)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	secretPath := filepath.Join(secretDir, "artifact-url-secret")
	if err := os.Symlink(target, secretPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	service := NewArtifactService(dataDir)
	if _, err := service.Sign("node_secret", "artifact1", "report.txt", strings.Repeat("a", 64), 12345); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("symlink secret error = %v", err)
	}
}

func TestVerifySignatureRejectsTamperedParameters(t *testing.T) {
	service := NewArtifactService(t.TempDir())
	signature, err := service.Sign("node_verify", "artifact1", "report.txt", strings.Repeat("a", 64), 12345)
	if err != nil {
		t.Fatal(err)
	}
	if !service.Verify("node_verify", "artifact1", "report.txt", strings.Repeat("a", 64), signature, 12345) {
		t.Fatal("legitimate parameters failed signature verification")
	}
	if service.Verify("node_other", "artifact1", "report.txt", strings.Repeat("a", 64), signature, 12345) {
		t.Fatal("a different node passed verification with someone else's signature")
	}
	if service.Verify("node_verify", "artifact1", "report.txt", strings.Repeat("b", 64), signature, 12345) {
		t.Fatal("tampered sha256 passed signature verification")
	}
	if service.Verify("node_verify", "artifact1", "report.txt", strings.Repeat("a", 64), signature, 12346) {
		t.Fatal("tampered expiry passed signature verification")
	}
}
