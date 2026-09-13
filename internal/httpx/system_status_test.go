package httpx

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/uvwt/nexusdock/internal/buildinfo"
	"github.com/uvwt/nexusdock/internal/config"
)

// 系统状态必须如实上报构建版本与修订，未注入时回落到 buildinfo 缺省值，
// 便于在生产环境直接确认运行的是哪一次构建。
func TestSystemStatusReportsBuildVersionAndRevision(t *testing.T) {
	h := newTestHandler(t, config.Config{})

	res := httptest.NewRecorder()
	h.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/v1/system/status", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("system status = %d body=%s", res.Code, res.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["version"] != buildinfo.Version || body["revision"] != buildinfo.Revision {
		t.Fatalf("system status build info = %#v, want version %q revision %q", body, buildinfo.Version, buildinfo.Revision)
	}
}
