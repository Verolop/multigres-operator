//go:build e2e

package framework

import (
	"bytes"
	"testing"
)

func TestSafeDiagnosticName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "kubernetes name", input: "e2e-ns-1234", want: "e2e-ns-1234"},
		{name: "path separators", input: "../secret/name", want: "_secret_name"},
		{name: "dot segment", input: "..", want: "unnamed"},
		{name: "empty", input: "", want: "unnamed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := safeDiagnosticName(tt.input); got != tt.want {
				t.Fatalf("safeDiagnosticName(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestResourceStatusOutputExcludesSpecAndAnnotations(t *testing.T) {
	t.Parallel()
	input := []byte(`{
		"items": [{
			"apiVersion": "multigres.com/v1alpha1",
			"kind": "Shard",
			"metadata": {
				"name": "test-shard",
				"namespace": "e2e-ns-1234",
				"generation": 2,
				"annotations": {"internal.example/context": "private"}
			},
			"spec": {"password": "do-not-copy"},
			"status": {"podRoles": {"pooler-0": "PRIMARY"}}
		}]
	}`)
	output, err := resourceStatusOutput(input)
	if err != nil {
		t.Fatalf("resourceStatusOutput: %v", err)
	}
	for _, excluded := range [][]byte{
		[]byte(`"spec"`),
		[]byte(`"annotations"`),
		[]byte("do-not-copy"),
		[]byte("private"),
	} {
		if bytes.Contains(output, excluded) {
			t.Errorf("output contains excluded context %q: %s", excluded, output)
		}
	}
	if !bytes.Contains(output, []byte(`"pooler-0": "PRIMARY"`)) {
		t.Fatalf("output does not contain pod role status: %s", output)
	}
}
