package httpx

import (
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/uvwt/nexusdock/internal/agentdock"
)

func TestRuntimeBridgeErrorPreservesRemoteErrorDetails(t *testing.T) {
	remote := &agentdock.RemoteError{
		Code: "INVALID_LIMIT", Message: "limit must be an integer between 0 and 200",
		Category: "validation", Retryable: true,
		Details: map[string]any{"minimum": float64(0), "maximum": float64(200)},
	}

	converted := runtimeBridgeError(remote)
	if converted.Code != "AGENTDOCK_RUNTIME_REQUEST_FAILED" || converted.UpstreamCode != remote.Code {
		t.Fatalf("converted codes = %#v", converted)
	}
	if converted.Status != http.StatusBadRequest || converted.Category != remote.Category || converted.Retryable != remote.Retryable {
		t.Fatalf("converted semantics = %#v", converted)
	}
	if !reflect.DeepEqual(converted.Details, remote.Details) {
		t.Fatalf("details = %#v, want %#v", converted.Details, remote.Details)
	}

	payload := runtimeUnavailablePayload(converted)
	detail := payload["error"].(map[string]any)
	if detail["upstream_code"] != remote.Code || detail["category"] != remote.Category || detail["retryable"] != true {
		t.Fatalf("payload detail = %#v", detail)
	}
	if !reflect.DeepEqual(detail["details"], remote.Details) {
		t.Fatalf("payload details = %#v", detail["details"])
	}
	if runtimeErrorHTTPStatus(converted) != http.StatusBadRequest {
		t.Fatalf("status = %d", runtimeErrorHTTPStatus(converted))
	}
}

func TestRuntimeBridgeErrorMapsRemoteCategories(t *testing.T) {
	for _, test := range []struct {
		category string
		want     int
	}{
		{category: "validation", want: http.StatusBadRequest},
		{category: "not_found", want: http.StatusNotFound},
		{category: "conflict", want: http.StatusConflict},
		{category: "runtime", want: http.StatusInternalServerError},
	} {
		t.Run(test.category, func(t *testing.T) {
			converted := runtimeBridgeError(&agentdock.RemoteError{Code: "UPSTREAM", Message: "failed", Category: test.category})
			if got := runtimeErrorHTTPStatus(converted); got != test.want {
				t.Fatalf("status = %d, want %d", got, test.want)
			}
		})
	}
}

func TestRuntimeBridgeErrorKeepsTransportFailuresUnavailable(t *testing.T) {
	converted := runtimeBridgeError(agentdock.ErrNodeOffline)
	if converted.Code != "AGENTDOCK_RUNTIME_UNREACHABLE" || converted.UpstreamCode != "" {
		t.Fatalf("converted = %#v", converted)
	}
	if got := runtimeErrorHTTPStatus(converted); got != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", got, http.StatusServiceUnavailable)
	}
}

func TestRuntimeUnavailableRecognizesRuntimeError(t *testing.T) {
	if !isRuntimeUnavailable(agentDockRuntimeError{Code: "AGENTDOCK_RUNTIME_UNREACHABLE"}) {
		t.Fatal("agentDockRuntimeError should be recognized")
	}
	if isRuntimeUnavailable(errors.New("other")) {
		t.Fatal("plain error should not be recognized")
	}
}

func TestRuntimeBridgeErrorClassifiesContractErrors(t *testing.T) {
	contractErr := &agentdock.ContractError{
		Node: "node1", Operation: "GET /internal/runtime/tasks",
		Field: "tasks[0].status", Reason: "非法枚举值 \"done\"",
	}
	converted := runtimeBridgeError(contractErr)
	if converted.Code != "AGENTDOCK_RUNTIME_BAD_RESPONSE" {
		t.Fatalf("code = %q", converted.Code)
	}
	if converted.Status != http.StatusBadGateway || runtimeErrorHTTPStatus(converted) != http.StatusBadGateway {
		t.Fatalf("status = %d", converted.Status)
	}
	// 错误信息必须保留节点、上游方法与字段路径，便于定位契约漂移。
	for _, fragment := range []string{"node1", "GET /internal/runtime/tasks", "tasks[0].status"} {
		if !strings.Contains(converted.Message, fragment) {
			t.Fatalf("message %q 缺少定位上下文 %q", converted.Message, fragment)
		}
	}
}
