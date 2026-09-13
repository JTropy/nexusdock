package agentdock

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	protocol "github.com/uvwt/agentdock-protocol"
)

// ArtifactChunk 是 AgentDock 私有 artifact.read Bridge 操作在 Nexus 边界内的强类型结果。
type ArtifactChunk struct {
	ArtifactID string    `json:"artifact_id"`
	Filename   string    `json:"filename"`
	MIMEType   string    `json:"mime_type"`
	Size       int64     `json:"size_bytes"`
	SHA256     string    `json:"sha256"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	Archive    bool      `json:"archive"`
	Width      int       `json:"width,omitempty"`
	Height     int       `json:"height,omitempty"`
	Offset     int64     `json:"offset"`
	NextOffset int64     `json:"next_offset"`
	DataBase64 string    `json:"data_base64"`
	EOF        bool      `json:"eof"`
}

func (h *Hub) ReadArtifactChunk(ctx context.Context, nodeID, artifactID string, offset int64, maxBytes int) (ArtifactChunk, error) {
	result, err := h.Invoke(ctx, nodeID, protocol.OperationArtifactRead, map[string]any{
		"artifact_id": artifactID,
		"offset":      offset,
		"max_bytes":   maxBytes,
	})
	if err != nil {
		return ArtifactChunk{}, err
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return ArtifactChunk{}, fmt.Errorf("编码 AgentDock Artifact 分块结果: %w", err)
	}
	var chunk ArtifactChunk
	if err := json.Unmarshal(encoded, &chunk); err != nil {
		return ArtifactChunk{}, fmt.Errorf("解析 AgentDock Artifact 分块结果: %w", err)
	}
	return chunk, nil
}

// maxConcurrentArtifactDownloadsPerNode 限制单节点的并发代理下载数，防止慢节点占满 Nexus 出口。
const maxConcurrentArtifactDownloadsPerNode = 2

// ArtifactService 持有 Nexus 侧 Artifact 公开下载的两类独立状态：
// HMAC 签名密钥（Nexus 数据目录 secrets 下懒加载、原子创建）与每节点并发下载预算。
// 它不感知 HTTP 协议；下载 URL 的拼装与响应映射仍留在 httpx 的下载路由中。
type ArtifactService struct {
	dataDir string

	secretMu sync.Mutex

	downloadsMu sync.Mutex
	downloads   map[string]int
}

func NewArtifactService(dataDir string) *ArtifactService {
	return &ArtifactService{dataDir: strings.TrimSpace(dataDir)}
}

// Sign 返回 Artifact 公开下载参数的 HMAC-SHA256 签名（hex 编码）。
func (s *ArtifactService) Sign(nodeID, artifactID, filename, sha string, expires int64) (string, error) {
	secret, err := s.signingSecret()
	if err != nil {
		return "", err
	}
	return signArtifactURL(secret, nodeID, artifactID, filename, sha, expires), nil
}

// Verify 以恒定时间比较校验下载请求携带的签名；签名密钥读取失败按校验失败处理。
func (s *ArtifactService) Verify(nodeID, artifactID, filename, sha, signature string, expires int64) bool {
	secret, err := s.signingSecret()
	if err != nil {
		return false
	}
	return hmac.Equal([]byte(signArtifactURL(secret, nodeID, artifactID, filename, sha, expires)), []byte(signature))
}

// AcquireDownload 尝试占用一个节点的并发下载名额；每节点预算独立，互不影响。
func (s *ArtifactService) AcquireDownload(nodeID string) bool {
	s.downloadsMu.Lock()
	defer s.downloadsMu.Unlock()
	if s.downloads == nil {
		s.downloads = make(map[string]int)
	}
	if s.downloads[nodeID] >= maxConcurrentArtifactDownloadsPerNode {
		return false
	}
	s.downloads[nodeID]++
	return true
}

// ReleaseDownload 归还一个节点的并发下载名额。
func (s *ArtifactService) ReleaseDownload(nodeID string) {
	s.downloadsMu.Lock()
	defer s.downloadsMu.Unlock()
	if s.downloads[nodeID] <= 1 {
		delete(s.downloads, nodeID)
		return
	}
	s.downloads[nodeID]--
}

// signingSecret 懒加载签名密钥：多进程/多 goroutine 并发首次创建时通过临时文件
// 加原子 link 保证只有一个密钥被落盘，且目录与文件权限分别收紧到 0700/0600。
func (s *ArtifactService) signingSecret() ([]byte, error) {
	s.secretMu.Lock()
	defer s.secretMu.Unlock()
	if s.dataDir == "" {
		return nil, errors.New("NEXUS_DATA_DIR is required for Artifact URL signing")
	}
	secretPath := filepath.Join(s.dataDir, "secrets", "artifact-url-secret")
	secretDir := filepath.Dir(secretPath)
	if err := os.MkdirAll(secretDir, 0o700); err != nil {
		return nil, fmt.Errorf("create NexusDock secrets directory: %w", err)
	}
	if err := os.Chmod(secretDir, 0o700); err != nil {
		return nil, fmt.Errorf("secure NexusDock secrets directory: %w", err)
	}
	if secret, err := readArtifactSigningSecret(secretPath); err == nil {
		return secret, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	secret := make([]byte, sha256.Size)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("generate NexusDock Artifact signing secret: %w", err)
	}
	tmp, err := os.CreateTemp(secretDir, ".artifact-url-secret-*")
	if err != nil {
		return nil, fmt.Errorf("create NexusDock Artifact signing secret temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("secure NexusDock Artifact signing secret temp file: %w", err)
	}
	if _, err := tmp.WriteString(hex.EncodeToString(secret) + "\n"); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("write NexusDock Artifact signing secret: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("sync NexusDock Artifact signing secret: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("close NexusDock Artifact signing secret: %w", err)
	}
	// os.Link 在目标已存在时失败，这里用它替代"检查后写入"，消除并发创建的竞态窗口。
	if err := os.Link(tmpPath, secretPath); err == nil {
		syncDirectoryBestEffort(secretDir)
		return secret, nil
	} else if !os.IsExist(err) {
		return nil, fmt.Errorf("publish NexusDock Artifact signing secret: %w", err)
	}
	// 另一个进程已经创建了密钥；读取既有密钥，保证所有进程使用同一份签名密钥。
	secret, err = readArtifactSigningSecret(secretPath)
	if err != nil {
		return nil, err
	}
	return secret, nil
}

func readArtifactSigningSecret(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("NexusDock Artifact signing secret must not be a symbolic link")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read NexusDock Artifact signing secret: %w", err)
	}
	secret, err := hex.DecodeString(strings.TrimSpace(string(data)))
	if err != nil || len(secret) != sha256.Size {
		return nil, errors.New("NexusDock Artifact signing secret has an invalid format")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("secure NexusDock Artifact signing secret: %w", err)
	}
	return secret, nil
}

func syncDirectoryBestEffort(path string) {
	dir, err := os.Open(path)
	if err != nil {
		return
	}
	_ = dir.Sync()
	_ = dir.Close()
}

func signArtifactURL(secret []byte, nodeID, artifactID, filename, sha string, expires int64) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = fmt.Fprintf(mac, "%s\x00%s\x00%s\x00%s\x00%d", nodeID, artifactID, filename, sha, expires)
	return hex.EncodeToString(mac.Sum(nil))
}

// ValidArtifactSHA 判断下载参数中的 sha256 是否为 64 位十六进制摘要。
func ValidArtifactSHA(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
