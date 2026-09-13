package agentdock

import (
	"errors"
	"reflect"
	"testing"

	protocol "github.com/uvwt/agentdock-protocol"
)

func TestToolContractHashIgnoresSchemaPresentationOnly(t *testing.T) {
	left := ToolDescriptor{
		Name: "exec_command",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"description": map[string]any{"type": "string", "description": "actual parameter"},
				"mode":        map[string]any{"type": "string", "title": "Mode", "description": "macOS text", "enum": []any{"host", "wsl"}},
			},
			"required": []any{"mode", "description"},
		},
	}
	right, err := cloneToolDescriptor(left)
	if err != nil {
		t.Fatal(err)
	}
	right.InputSchema["properties"].(map[string]any)["mode"].(map[string]any)["title"] = "Execution mode"
	right.InputSchema["properties"].(map[string]any)["mode"].(map[string]any)["description"] = "Windows text"
	right.InputSchema["required"] = []any{"description", "mode"}
	right.InputSchema["properties"].(map[string]any)["mode"].(map[string]any)["enum"] = []any{"wsl", "host"}

	leftHash, err := ToolContractHash(left)
	if err != nil {
		t.Fatal(err)
	}
	rightHash, err := ToolContractHash(right)
	if err != nil {
		t.Fatal(err)
	}
	if leftHash != rightHash {
		t.Fatalf("presentation/order-only changes altered semantic hash: %s != %s", leftHash, rightHash)
	}

	withoutRealDescriptionParameter, err := cloneToolDescriptor(left)
	if err != nil {
		t.Fatal(err)
	}
	delete(withoutRealDescriptionParameter.InputSchema["properties"].(map[string]any), "description")
	removedHash, err := ToolContractHash(withoutRealDescriptionParameter)
	if err != nil {
		t.Fatal(err)
	}
	if removedHash == leftHash {
		t.Fatal("a real parameter named description was incorrectly treated as presentation metadata")
	}
}

func TestToolContractHashIgnoresToolPresentationMetadata(t *testing.T) {
	base := platformContractDescriptor("file_edit", map[string]any{
		"path": map[string]any{"type": "string"},
	}, []any{"path"})
	presented, err := cloneToolDescriptor(base)
	if err != nil {
		t.Fatal(err)
	}
	presented.Meta = map[string]any{"ui": map[string]any{"resourceUri": protocol.FileChangeUIResourceURI}}
	presented.Annotations = map[string]any{
		"readOnlyHint": false, "destructiveHint": true, "idempotentHint": false, "openWorldHint": false,
	}

	baseHash, err := ToolContractHash(base)
	if err != nil {
		t.Fatal(err)
	}
	presentedHash, err := ToolContractHash(presented)
	if err != nil {
		t.Fatal(err)
	}
	if baseHash != presentedHash {
		t.Fatalf("presentation metadata altered execution hash: %s != %s", baseHash, presentedHash)
	}

	withExecutionMeta, err := cloneToolDescriptor(presented)
	if err != nil {
		t.Fatal(err)
	}
	withExecutionMeta.Meta["file_arg_rewrite_paths"] = []any{"path"}
	executionMetaHash, err := ToolContractHash(withExecutionMeta)
	if err != nil {
		t.Fatal(err)
	}
	if executionMetaHash == presentedHash {
		t.Fatal("non-UI _meta was incorrectly ignored by the execution hash")
	}
}

func TestMergeFleetToolDescriptorsMergesPresentationConservatively(t *testing.T) {
	old := platformContractDescriptor("file_edit", map[string]any{
		"path": map[string]any{"type": "string"},
	}, []any{"path"})
	old.Meta = map[string]any{
		"shared": "same",
		"ui":     map[string]any{"resourceUri": protocol.FileChangeUIResourceURI},
	}

	newDescriptor, err := cloneToolDescriptor(old)
	if err != nil {
		t.Fatal(err)
	}
	delete(newDescriptor.Meta, "ui")
	newDescriptor.Annotations = map[string]any{
		"readOnlyHint": false, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false,
	}

	merged, accepted, err := mergeFleetToolDescriptors([]ToolDescriptor{old, newDescriptor})
	if err != nil {
		t.Fatal(err)
	}
	if len(accepted) != 1 {
		t.Fatalf("same execution schema should have one accepted hash: %#v", accepted)
	}
	if merged.Meta["shared"] != "same" || merged.Meta["ui"] != nil {
		t.Fatalf("merged meta = %#v", merged.Meta)
	}
	wantAnnotations := map[string]any{
		"readOnlyHint": false, "destructiveHint": true, "idempotentHint": false, "openWorldHint": true,
	}
	if !reflect.DeepEqual(merged.Annotations, wantAnnotations) {
		t.Fatalf("merged annotations = %#v, want %#v", merged.Annotations, wantAnnotations)
	}

	updatedOld, err := cloneToolDescriptor(old)
	if err != nil {
		t.Fatal(err)
	}
	updatedOld.Annotations = map[string]any{
		"readOnlyHint": false, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false,
	}
	updatedNew, err := cloneToolDescriptor(updatedOld)
	if err != nil {
		t.Fatal(err)
	}
	converged, _, err := mergeFleetToolDescriptors([]ToolDescriptor{updatedOld, updatedNew})
	if err != nil {
		t.Fatal(err)
	}
	ui, ok := converged.Meta["ui"].(map[string]any)
	if !ok || ui["resourceUri"] != protocol.FileChangeUIResourceURI {
		t.Fatalf("converged ui meta = %#v", converged.Meta["ui"])
	}
	if converged.Annotations["destructiveHint"] != false || converged.Annotations["idempotentHint"] != true || converged.Annotations["openWorldHint"] != false {
		t.Fatalf("converged annotations = %#v", converged.Annotations)
	}

	firstPresentation, err := cloneToolDescriptor(updatedOld)
	if err != nil {
		t.Fatal(err)
	}
	secondPresentation, err := cloneToolDescriptor(updatedOld)
	if err != nil {
		t.Fatal(err)
	}
	secondPresentation.Meta["ui"] = map[string]any{"resourceUri": protocol.TaskProgressUIResourceURI}

	mixedPresentation, _, err := mergeFleetToolDescriptors([]ToolDescriptor{firstPresentation, secondPresentation})
	if err != nil {
		t.Fatal(err)
	}
	if mixedPresentation.Meta["ui"] != nil {
		t.Fatalf("mixed presentation bindings must not publish shared UI: %#v", mixedPresentation.Meta)
	}

}

func TestMergeFleetToolDescriptorsSupportsPlatformOptionalProperties(t *testing.T) {
	tests := []struct {
		name string
		mac  ToolDescriptor
		win  ToolDescriptor
		want []string
	}{
		{
			name: "exec_command",
			mac: platformContractDescriptor("exec_command", map[string]any{
				"command": map[string]any{"type": "string"},
				"workdir": map[string]any{"type": "string", "description": "host path"},
			}, []any{"command"}),
			win: platformContractDescriptor("exec_command", map[string]any{
				"command":          map[string]any{"type": "string"},
				"workdir":          map[string]any{"type": "string", "description": "Windows or WSL path"},
				"runtime":          map[string]any{"type": "string", "enum": []any{"windows", "wsl"}},
				"wsl_distribution": map[string]any{"type": "string"},
			}, []any{"command"}),
			want: []string{"runtime", "wsl_distribution"},
		},
		{
			name: "list_files",
			mac: platformContractDescriptor("list_files", map[string]any{
				"path": map[string]any{"type": "string", "description": "host path"},
			}, []any{"path"}),
			win: platformContractDescriptor("list_files", map[string]any{
				"path":             map[string]any{"type": "string", "description": "Windows or WSL path"},
				"runtime":          map[string]any{"type": "string", "enum": []any{"windows", "wsl"}},
				"wsl_distribution": map[string]any{"type": "string"},
			}, []any{"path"}),
			want: []string{"runtime", "wsl_distribution"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mac, err := cloneToolDescriptor(tt.mac)
			if err != nil {
				t.Fatal(err)
			}
			win, err := cloneToolDescriptor(tt.win)
			if err != nil {
				t.Fatal(err)
			}
			mac.OutputSchema = map[string]any{
				"type":                 "object",
				"properties":           map[string]any{"result": map[string]any{"type": "string"}},
				"additionalProperties": false,
			}
			win.OutputSchema = map[string]any{
				"type": "object",
				"properties": map[string]any{
					"result":           map[string]any{"type": "string"},
					"runtime":          map[string]any{"type": "string"},
					"wsl_distribution": map[string]any{"type": "string"},
				},
				"additionalProperties": false,
			}

			merged, accepted, err := mergeFleetToolDescriptors([]ToolDescriptor{mac, win})
			if err != nil {
				t.Fatal(err)
			}
			properties := merged.InputSchema["properties"].(map[string]any)
			for _, property := range tt.want {
				if _, ok := properties[property]; !ok {
					t.Fatalf("merged schema missing optional platform property %s: %#v", property, properties)
				}
			}
			outputProperties := merged.OutputSchema["properties"].(map[string]any)
			for _, property := range tt.want {
				if _, ok := outputProperties[property]; !ok {
					t.Fatalf("merged output schema missing optional platform property %s: %#v", property, outputProperties)
				}
			}
			if required := merged.InputSchema["required"]; !reflect.DeepEqual(required, []string{tt.mac.InputSchema["required"].([]any)[0].(string)}) {
				t.Fatalf("required = %#v", required)
			}
			if len(accepted) != 2 {
				t.Fatalf("accepted hashes = %#v", accepted)
			}
		})
	}
}

func TestMergeFleetToolDescriptorsRejectsValidationDrift(t *testing.T) {
	base := platformContractDescriptor("exec_command", map[string]any{
		"command": map[string]any{"type": "string"},
		"mode":    map[string]any{"type": "string", "enum": []any{"host", "wsl"}},
	}, []any{"command"})

	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "shared type", mutate: func(properties map[string]any) { properties["command"].(map[string]any)["type"] = "array" }},
		{name: "shared enum", mutate: func(properties map[string]any) {
			properties["mode"].(map[string]any)["enum"] = []any{"host", "windows"}
		}},
		{name: "additional properties", mutate: func(properties map[string]any) {}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			other, err := cloneToolDescriptor(base)
			if err != nil {
				t.Fatal(err)
			}
			if tt.name == "additional properties" {
				other.InputSchema["additionalProperties"] = true
			} else {
				tt.mutate(other.InputSchema["properties"].(map[string]any))
			}
			_, _, err = mergeFleetToolDescriptors([]ToolDescriptor{base, other})
			if !errors.Is(err, errIncompatibleToolContract) {
				t.Fatalf("error = %v, want incompatible contract", err)
			}
		})
	}

	requiredDrift, err := cloneToolDescriptor(base)
	if err != nil {
		t.Fatal(err)
	}
	requiredDrift.InputSchema["required"] = []any{"command", "mode"}
	if _, _, err := mergeFleetToolDescriptors([]ToolDescriptor{base, requiredDrift}); !errors.Is(err, errIncompatibleToolContract) {
		t.Fatalf("required drift error = %v", err)
	}

	providerOnlyRequired, err := cloneToolDescriptor(base)
	if err != nil {
		t.Fatal(err)
	}
	providerOnlyRequired.InputSchema["properties"].(map[string]any)["runtime"] = map[string]any{"type": "string"}
	providerOnlyRequired.InputSchema["required"] = []any{"command", "runtime"}
	if _, _, err := mergeFleetToolDescriptors([]ToolDescriptor{base, providerOnlyRequired}); !errors.Is(err, errIncompatibleToolContract) {
		t.Fatalf("provider-only required property error = %v", err)
	}
}

func platformContractDescriptor(name string, properties map[string]any, required []any) ToolDescriptor {
	return ToolDescriptor{
		Name: name,
		InputSchema: map[string]any{
			"type":                 "object",
			"properties":           properties,
			"required":             required,
			"additionalProperties": false,
		},
	}
}
