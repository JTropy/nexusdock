package httpx

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	protocol "github.com/uvwt/agentdock-protocol"
	"github.com/uvwt/agentdock-protocol/mcpapps"
	"github.com/uvwt/nexusdock/internal/agentdock"
)

func TestImageAppPreservesNodeContract(t *testing.T) {
	descriptor := agentdock.ToolDescriptor{Name: "view_image", InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"}, Meta: map[string]any{"trace": "keep"}}
	for _, enabled := range []bool{true, false} {
		tool := nodeMCPToolWithApps(descriptor, enabled)
		if !reflect.DeepEqual(tool.OutputSchema, descriptor.OutputSchema) {
			t.Fatal("output schema changed")
		}
		if enabled && tool.Meta["ui"] == nil {
			t.Fatal("missing image component")
		}
		if !enabled && (tool.Meta["ui"] != nil || tool.Meta["openai/outputTemplate"] != nil) {
			t.Fatal("disabled Apps exposes component")
		}
		if tool.Meta["trace"] != "keep" || descriptor.Meta["ui"] != nil {
			t.Fatal("upstream metadata changed")
		}
	}
	metadata := map[string]any{"image": map[string]any{"width": float64(2)}}
	result := map[string]any{"isError": false, "structuredContent": metadata, "content": []map[string]any{{"type": "image", "mimeType": "image/png", "data": "AQID"}}}
	got, err := gatewayToolResult("view_image", result, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.StructuredContent, metadata) || len(got.Content) != 1 {
		t.Fatal("upstream result changed")
	}
	img, ok := got.Content[0].(*mcpsdk.ImageContent)
	if !ok || !bytes.Equal(img.Data, []byte{1, 2, 3}) {
		t.Fatal("pixels changed")
	}
	other := nodeMCPTool(agentdock.ToolDescriptor{Name: "other", InputSchema: map[string]any{"type": "object"}})
	if other.Meta["ui"] != nil {
		t.Fatal("unrelated tool gained image component")
	}
}

func TestImageAppResourceAvailableOnlyWhenEnabled(t *testing.T) {
	server := &Server{mcpAppsEnabledState: true}
	resources, err := server.publishedMCPAppResourceURIs(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	uri := protocol.ImageUIResourceURI
	if _, ok := resources[uri]; !ok {
		t.Fatal("missing image resource")
	}
	read, err := server.readPublishedMCPAppResource(t.Context(), uri)
	if err != nil {
		t.Fatal(err)
	}
	if len(read.Contents) != 1 || !strings.Contains(read.Contents[0].Text, "imageIds") {
		t.Fatal("missing image bridge")
	}
	if read.Contents[0].Text != mcpapps.HTML("view_image", "Image") {
		t.Fatal("renderer differs from shared component")
	}
	server.mcpAppsEnabledState = false
	if _, err := server.readPublishedMCPAppResource(t.Context(), uri); err == nil {
		t.Fatal("disabled image resource readable")
	}
}

func TestImageAppDoesNotDuplicateNewNodePresentation(t *testing.T) {
	server := &Server{mcpAppsEnabledState: true}
	upstream := map[string]any{"isError": false, "content": []map[string]any{{"type": "image", "mimeType": "image/png", "data": "AQID"}, {"type": "text", "text": mcpapps.ImageResultText}}, "_meta": map[string]any{"ui": map[string]any{"resourceUri": protocol.ImageUIResourceURI}, "openai/outputTemplate": protocol.ImageUIResourceURI}}
	result, err := server.gatewayToolResult("view_image", upstream, nil)
	if err != nil || len(result.Content) != 2 {
		t.Fatalf("duplicated node presentation: %v", err)
	}
	server.mcpAppsEnabledState = false
	result, err = server.gatewayToolResult("view_image", upstream, nil)
	if err != nil || result.Meta["ui"] != nil || result.Meta["openai/outputTemplate"] != nil {
		t.Fatal("disabled Apps leaked node presentation")
	}
	tool := nodeMCPToolWithApps(agentdock.ToolDescriptor{Name: "view_image", InputSchema: map[string]any{"type": "object"}, Meta: upstream["_meta"].(map[string]any)}, false)
	if tool.Meta["openai/outputTemplate"] != nil {
		t.Fatal("disabled tool leaked image template")
	}
}

func TestImageAppResultBindsOnlySuccessfulImages(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, failed := range []bool{false, true} {
			server := &Server{mcpAppsEnabledState: enabled}
			result, err := server.gatewayToolResult("view_image", map[string]any{"isError": failed, "content": []map[string]any{{"type": "image", "mimeType": "image/png", "data": "AQID"}}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if enabled && !failed {
				if result.Meta["ui"] == nil || len(result.Content) != 2 {
					t.Fatal("successful image missing presentation binding")
				}
			} else if result.Meta["ui"] != nil || len(result.Content) != 1 {
				t.Fatal("disabled or failed tool gained image presentation")
			}
		}
	}
}
