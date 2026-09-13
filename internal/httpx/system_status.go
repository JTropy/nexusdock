package httpx

import (
	"net/http"

	"github.com/uvwt/nexusdock/internal/buildinfo"
)

func (s *Server) systemStatus(w http.ResponseWriter, r *http.Request) {
	recallRepoDir := s.store.Root()
	status := map[string]any{
		"ok":              true,
		"service":         "nexusdock",
		"version":         buildinfo.Version,
		"revision":        buildinfo.Revision,
		"database":        "unavailable",
		"schema_version":  0,
		"nexus_data_dir":  s.cfg.NexusDataDir,
		"recall_repo_dir": recallRepoDir,
	}
	if s.db == nil {
		status["ok"] = false
		writeJSON(w, http.StatusServiceUnavailable, status)
		return
	}
	var check string
	if err := s.db.QueryRowContext(r.Context(), `PRAGMA quick_check`).Scan(&check); err != nil || check != "ok" {
		status["ok"] = false
		status["database"] = check
		if check == "" {
			status["database"] = "error"
		}
		writeJSON(w, http.StatusServiceUnavailable, status)
		return
	}
	status["database"] = "ok"
	// schema_version 返回控制库真实写入的 Schema 版本（PRAGMA user_version），
	// 启动时 EnsureSchema 已把它推进到 core.CurrentSchemaVersion，这里如实上报。
	var schemaVersion int
	if err := s.db.QueryRowContext(r.Context(), `PRAGMA user_version`).Scan(&schemaVersion); err == nil {
		status["schema_version"] = schemaVersion
	}
	writeJSON(w, http.StatusOK, status)
}
