package httpx

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	protocol "github.com/uvwt/agentdock-protocol"
	"github.com/uvwt/nexusdock/internal/agentdock"
)

const (
	maxProxiedArtifactBytes  = 512 << 20
	artifactChunkTimeout     = 30 * time.Second
	maxArtifactChunkRequests = (maxProxiedArtifactBytes + protocol.MaxArtifactChunkBytes - 1) / protocol.MaxArtifactChunkBytes
	// 整体 deadline 刻意约束慢节点或恶意节点：即使每个分块都小于 artifactChunkTimeout，
	// 总时长也有硬上限。
	artifactDownloadTimeout = 30 * time.Minute
)

func (s *Server) decorateArtifactToolResult(nodeID string, envelope map[string]any) error {
	if strings.TrimSpace(s.cfg.PublicURL) == "" || envelope == nil {
		return nil
	}
	structured, ok := envelope["structuredContent"].(map[string]any)
	if !ok {
		return nil
	}
	artifactID, _ := structured["artifact_id"].(string)
	filename, _ := structured["filename"].(string)
	sha, _ := structured["sha256"].(string)
	sha = strings.ToLower(strings.TrimSpace(sha))
	expiresText, _ := structured["expires_at"].(string)
	if artifactID == "" || filename == "" || !agentdock.ValidArtifactSHA(sha) || expiresText == "" {
		return nil
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, expiresText)
	if err != nil || !expiresAt.After(time.Now().UTC()) {
		return nil
	}
	publicURL, err := s.signedArtifactURL(nodeID, artifactID, filename, sha, expiresAt.Unix())
	if err != nil {
		return err
	}
	structured["url"] = publicURL
	structured["download_via"] = "nexusdock"
	refreshEnvelopeTextContent(envelope, structured)
	return nil
}

func refreshEnvelopeTextContent(envelope, structured map[string]any) {
	content, ok := envelope["content"].([]any)
	if !ok {
		return
	}
	for _, item := range content {
		block, ok := item.(map[string]any)
		if ok && block["type"] == "text" {
			block["text"] = prettyJSON(structured)
			return
		}
	}
}

// signedArtifactURL 组装签名下载 URL。签名密钥与并发下载状态由组合根注入的
// ArtifactService 持有；这里只负责 URL 形状与公网 origin 的拼接。
func (s *Server) signedArtifactURL(nodeID, artifactID, filename, sha string, expires int64) (string, error) {
	if s.artifacts == nil {
		return "", errors.New("Artifact 签名能力未注入")
	}
	signature, err := s.artifacts.Sign(nodeID, artifactID, filename, sha, expires)
	if err != nil {
		return "", err
	}
	query := url.Values{
		"expires": {strconv.FormatInt(expires, 10)},
		"sha256":  {sha},
		"sig":     {signature},
	}
	base := strings.TrimRight(strings.TrimSpace(s.cfg.PublicURL), "/")
	return base + "/artifacts/public/" + url.PathEscape(nodeID) + "/" + url.PathEscape(artifactID) + "/" + url.PathEscape(filename) + "?" + query.Encode(), nil
}

func (s *Server) servePublicArtifact(w http.ResponseWriter, r *http.Request) {
	setArtifactPublicHeaders(w.Header())
	if s.agentDockHub == nil {
		http.NotFound(w, r)
		return
	}
	if s.artifacts == nil {
		writeError(w, http.StatusInternalServerError, "ARTIFACT_SECRET_FAILED", "Artifact download is temporarily unavailable")
		return
	}
	nodeID := strings.TrimSpace(r.PathValue("nodeID"))
	artifactID := strings.TrimSpace(r.PathValue("artifactID"))
	filename := strings.TrimSpace(r.PathValue("filename"))
	expires, parseErr := strconv.ParseInt(r.URL.Query().Get("expires"), 10, 64)
	sha := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("sha256")))
	signature := strings.TrimSpace(r.URL.Query().Get("sig"))
	if nodeID == "" || artifactID == "" || filename == "" || parseErr != nil || expires <= 0 || !agentdock.ValidArtifactSHA(sha) || signature == "" {
		http.NotFound(w, r)
		return
	}
	if time.Now().UTC().Unix() > expires {
		http.Error(w, http.StatusText(http.StatusGone), http.StatusGone)
		return
	}
	if !s.artifacts.Verify(nodeID, artifactID, filename, sha, signature, expires) {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodGet {
		if !s.artifacts.AcquireDownload(nodeID) {
			w.Header().Set("Retry-After", "5")
			writeError(w, http.StatusTooManyRequests, "ARTIFACT_DOWNLOAD_BUSY", "Too many concurrent Artifact downloads for this node")
			return
		}
		defer s.artifacts.ReleaseDownload(nodeID)
	}

	downloadCtx, cancel := context.WithTimeout(r.Context(), artifactDownloadTimeout)
	defer cancel()

	if r.Method == http.MethodHead {
		chunk, readErr := s.readArtifactChunk(downloadCtx, nodeID, artifactID, 0, 0)
		if readErr != nil {
			s.writeArtifactBridgeError(w, readErr)
			return
		}
		if err := validateArtifactChunk(chunk, artifactID, filename, sha, expires, 0, true); err != nil {
			writeError(w, http.StatusBadGateway, "ARTIFACT_NODE_RESPONSE_INVALID", err.Error())
			return
		}
		if chunk.Size > maxProxiedArtifactBytes {
			writeError(w, http.StatusRequestEntityTooLarge, "ARTIFACT_TOO_LARGE", "Artifact exceeds the NexusDock proxy limit")
			return
		}
		setArtifactHeaders(w.Header(), chunk)
		w.WriteHeader(http.StatusOK)
		return
	}

	first, err := s.readArtifactChunk(downloadCtx, nodeID, artifactID, 0, protocol.MaxArtifactChunkBytes)
	if err != nil {
		s.writeArtifactBridgeError(w, err)
		return
	}
	if err := validateArtifactChunk(first, artifactID, filename, sha, expires, 0, false); err != nil {
		writeError(w, http.StatusBadGateway, "ARTIFACT_NODE_RESPONSE_INVALID", err.Error())
		return
	}
	if first.Size > maxProxiedArtifactBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "ARTIFACT_TOO_LARGE", "Artifact exceeds the NexusDock proxy limit")
		return
	}
	firstData, err := decodeArtifactChunkData(first, 0)
	if err != nil {
		writeError(w, http.StatusBadGateway, "ARTIFACT_NODE_RESPONSE_INVALID", err.Error())
		return
	}
	setArtifactHeaders(w.Header(), first)
	w.WriteHeader(http.StatusOK)

	hasher := sha256.New()
	_, _ = hasher.Write(firstData)
	pending := firstData
	offset := first.NextOffset
	chunk := first
	chunkRequests := 1
	for !chunk.EOF {
		if chunkRequests >= maxArtifactChunkRequests {
			s.artifactLogger().Error("AgentDock Artifact stream exceeded chunk request limit", "node_id", nodeID, "artifact_id", artifactID, "requests", chunkRequests)
			panic(http.ErrAbortHandler)
		}
		chunk, err = s.readArtifactChunk(downloadCtx, nodeID, artifactID, offset, protocol.MaxArtifactChunkBytes)
		chunkRequests++
		if err != nil {
			s.artifactLogger().Error("read AgentDock Artifact chunk", "node_id", nodeID, "artifact_id", artifactID, "offset", offset, "error", err)
			panic(http.ErrAbortHandler)
		}
		if chunk.Size != first.Size {
			s.artifactLogger().Error("AgentDock Artifact size changed between chunks", "node_id", nodeID, "artifact_id", artifactID, "offset", offset, "first_size", first.Size, "chunk_size", chunk.Size)
			panic(http.ErrAbortHandler)
		}
		if err := validateArtifactChunk(chunk, artifactID, filename, sha, expires, offset, false); err != nil {
			s.artifactLogger().Error("validate AgentDock Artifact chunk", "node_id", nodeID, "artifact_id", artifactID, "error", err)
			panic(http.ErrAbortHandler)
		}
		data, decodeErr := decodeArtifactChunkData(chunk, offset)
		if decodeErr != nil {
			s.artifactLogger().Error("decode AgentDock Artifact chunk", "node_id", nodeID, "artifact_id", artifactID, "error", decodeErr)
			panic(http.ErrAbortHandler)
		}
		_, _ = hasher.Write(data)
		if len(pending) > 0 {
			if _, err := w.Write(pending); err != nil {
				return
			}
		}
		pending = data
		offset = chunk.NextOffset
	}
	if offset != first.Size || hex.EncodeToString(hasher.Sum(nil)) != sha {
		s.artifactLogger().Error("AgentDock Artifact stream checksum mismatch", "node_id", nodeID, "artifact_id", artifactID, "bytes", offset)
		panic(http.ErrAbortHandler)
	}
	if len(pending) > 0 {
		_, _ = w.Write(pending)
	}
}

func (s *Server) readArtifactChunk(ctx context.Context, nodeID, artifactID string, offset int64, maxBytes int) (agentdock.ArtifactChunk, error) {
	chunkCtx, cancel := context.WithTimeout(ctx, artifactChunkTimeout)
	defer cancel()
	return s.agentDockHub.ReadArtifactChunk(chunkCtx, nodeID, artifactID, offset, maxBytes)
}

func decodeArtifactChunkData(chunk agentdock.ArtifactChunk, offset int64) ([]byte, error) {
	data, err := base64.StdEncoding.DecodeString(chunk.DataBase64)
	if err != nil {
		return nil, errors.New("AgentDock Artifact chunk payload is not valid base64")
	}
	if len(data) > protocol.MaxArtifactChunkBytes || chunk.NextOffset != offset+int64(len(data)) {
		return nil, errors.New("AgentDock Artifact chunk payload length does not match its offsets")
	}
	return data, nil
}

func validateArtifactChunk(chunk agentdock.ArtifactChunk, artifactID, filename, sha string, expires, offset int64, metadataOnly bool) error {
	if chunk.ArtifactID != artifactID || chunk.Filename != filename || strings.ToLower(chunk.SHA256) != sha || chunk.ExpiresAt.Unix() != expires {
		return errors.New("AgentDock Artifact metadata does not match the signed URL")
	}
	if chunk.Size < 0 || chunk.Offset != offset {
		return errors.New("AgentDock Artifact size or offset is invalid")
	}
	if chunk.NextOffset < chunk.Offset || chunk.NextOffset > chunk.Size {
		return errors.New("AgentDock Artifact next offset is invalid")
	}
	if !metadataOnly && !chunk.EOF && chunk.NextOffset <= chunk.Offset {
		return errors.New("AgentDock Artifact chunk did not advance the stream")
	}
	if !metadataOnly && chunk.EOF != (chunk.NextOffset == chunk.Size) {
		return errors.New("AgentDock Artifact end-of-stream marker is invalid")
	}
	if metadataOnly && chunk.DataBase64 != "" {
		return errors.New("AgentDock returned payload data for a metadata-only request")
	}
	return nil
}

func (s *Server) artifactLogger() *slog.Logger {
	if s.logger != nil {
		return s.logger
	}
	return slog.Default()
}

func setArtifactHeaders(headers http.Header, chunk agentdock.ArtifactChunk) {
	setArtifactPublicHeaders(headers)
	mimeType := strings.TrimSpace(chunk.MIMEType)
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	headers.Set("Content-Type", mimeType)
	headers.Set("Content-Length", strconv.FormatInt(chunk.Size, 10))
	headers.Set("Accept-Ranges", "none")
	headers.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": chunk.Filename}))
	headers.Set("Content-Security-Policy", "sandbox; default-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
}

func setArtifactPublicHeaders(headers http.Header) {
	headers.Set("Cache-Control", "private, no-store")
	headers.Set("Access-Control-Allow-Origin", "*")
	headers.Set("Cross-Origin-Resource-Policy", "cross-origin")
	headers.Set("X-Content-Type-Options", "nosniff")
}

func (s *Server) writeArtifactBridgeError(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	code := "ARTIFACT_PROXY_FAILED"
	if errors.Is(err, context.DeadlineExceeded) {
		status = http.StatusGatewayTimeout
	} else if errors.Is(err, agentdock.ErrNodeOffline) || errors.Is(err, agentdock.ErrNodeDisconnected) {
		status = http.StatusServiceUnavailable
		code = "ARTIFACT_NODE_OFFLINE"
	}
	writeError(w, status, code, err.Error())
}
