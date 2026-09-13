package httpx

import (
	"errors"
	"net/http"

	"github.com/uvwt/nexusdock/internal/agentdock"
)

type agentDockRuntimeError struct {
	Status       int            `json:"-"`
	Code         string         `json:"code"`
	Message      string         `json:"message"`
	UpstreamCode string         `json:"upstream_code,omitempty"`
	Category     string         `json:"category,omitempty"`
	Retryable    bool           `json:"retryable,omitempty"`
	Details      map[string]any `json:"details,omitempty"`
}

func (e agentDockRuntimeError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	if e.Code != "" {
		return e.Code
	}
	return "AgentDock Runtime API unavailable"
}

// runtimeBridgeError 把 internal/agentdock 的错误转换成带 HTTP 语义的 Runtime 视图错误。
// 契约错误单独成类：上游返回了 Nexus 无法理解的数据时必须显式暴露，而不是渲染成空列表。
func runtimeBridgeError(err error) agentDockRuntimeError {
	var contractErr *agentdock.ContractError
	if errors.As(err, &contractErr) {
		return agentDockRuntimeError{
			Status: http.StatusBadGateway, Code: "AGENTDOCK_RUNTIME_BAD_RESPONSE", Message: contractErr.Error(),
		}
	}
	if errors.Is(err, agentdock.ErrBridgeUnavailable) {
		return agentDockRuntimeError{
			Status: http.StatusServiceUnavailable, Code: "AGENTDOCK_CONNECTION_UNAVAILABLE", Message: err.Error(),
		}
	}
	if errors.Is(err, agentdock.ErrNodeNotFound) {
		return agentDockRuntimeError{
			Status: http.StatusNotFound, Code: "AGENTDOCK_NODE_NOT_FOUND", Message: err.Error(),
		}
	}
	var lookupErr *agentdock.NodeLookupError
	if errors.As(err, &lookupErr) {
		return agentDockRuntimeError{
			Status: http.StatusServiceUnavailable, Code: "AGENTDOCK_NODE_LOOKUP_FAILED", Message: lookupErr.Error(),
		}
	}
	var remote *agentdock.RemoteError
	if errors.As(err, &remote) {
		status := http.StatusInternalServerError
		switch remote.Category {
		case "validation":
			status = http.StatusBadRequest
		case "not_found":
			status = http.StatusNotFound
		case "conflict":
			status = http.StatusConflict
		}
		return agentDockRuntimeError{
			Status: status, Code: "AGENTDOCK_RUNTIME_REQUEST_FAILED", Message: remote.Error(),
			UpstreamCode: remote.Code, Category: remote.Category, Retryable: remote.Retryable, Details: remote.Details,
		}
	}
	return agentDockRuntimeError{Status: http.StatusServiceUnavailable, Code: "AGENTDOCK_RUNTIME_UNREACHABLE", Message: err.Error()}
}

func runtimeUnavailablePayload(err error) map[string]any {
	code := "AGENTDOCK_RUNTIME_UNAVAILABLE"
	message := "AgentDock Runtime API 不可用"
	var rtErr agentDockRuntimeError
	if err != nil {
		if converted, ok := err.(agentDockRuntimeError); ok {
			rtErr = converted
		} else if converted, ok := err.(*agentDockRuntimeError); ok {
			rtErr = *converted
		}
		if rtErr.Code != "" {
			code = rtErr.Code
		}
		if err.Error() != "" {
			message = err.Error()
		}
	}
	detail := map[string]any{"code": code, "message": message}
	if rtErr.UpstreamCode != "" {
		detail["upstream_code"] = rtErr.UpstreamCode
		detail["retryable"] = rtErr.Retryable
	}
	if rtErr.Category != "" {
		detail["category"] = rtErr.Category
	}
	if len(rtErr.Details) > 0 {
		detail["details"] = rtErr.Details
	}
	return map[string]any{"ok": false, "available": false, "source": "agentdock-runtime-api", "error": detail}
}

func runtimeErrorHTTPStatus(err error) int {
	status := http.StatusServiceUnavailable
	switch converted := err.(type) {
	case agentDockRuntimeError:
		status = converted.Status
	case *agentDockRuntimeError:
		status = converted.Status
	}
	switch status {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusConflict, http.StatusBadGateway, http.StatusInternalServerError:
		return status
	default:
		return http.StatusServiceUnavailable
	}
}

func isRuntimeUnavailable(err error) bool {
	var runtimeErr agentDockRuntimeError
	return errors.As(err, &runtimeErr)
}

// writeRuntimeUnavailable 统一输出 Runtime 视图的错误响应。
// Hub 返回的是原始错误，必须先经过 runtimeBridgeError 转换，code 与 HTTP 状态才有明确语义。
func writeRuntimeUnavailable(w http.ResponseWriter, err error) {
	runtimeErr := runtimeBridgeError(err)
	writeJSON(w, runtimeErrorHTTPStatus(runtimeErr), runtimeUnavailablePayload(runtimeErr))
}
