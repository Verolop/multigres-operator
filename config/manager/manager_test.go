package manager

import (
	"errors"
	"io"
	"os"
	"testing"

	"k8s.io/apimachinery/pkg/util/yaml"
)

func TestControllerManagerUsesNonOverlappingRollout(t *testing.T) {
	f, err := os.Open("manager.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			t.Errorf("close manager manifest: %v", err)
		}
	}()

	type manifest struct {
		Kind string `json:"kind"`
		Spec struct {
			Strategy map[string]any `json:"strategy"`
		} `json:"spec"`
	}

	decoder := yaml.NewYAMLOrJSONDecoder(f, 4096)
	for {
		var resource manifest
		if err := decoder.Decode(&resource); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatal(err)
		}
		if resource.Kind != "Deployment" {
			continue
		}
		if got := resource.Spec.Strategy["type"]; got != "Recreate" {
			t.Fatalf("controller-manager strategy = %q, want Recreate", got)
		}
		rollingUpdate, present := resource.Spec.Strategy["rollingUpdate"]
		if !present {
			t.Fatal("controller-manager strategy must explicitly clear rollingUpdate")
		}
		if rollingUpdate != nil {
			t.Fatalf("controller-manager rollingUpdate = %#v, want null", rollingUpdate)
		}
		return
	}

	t.Fatal("controller-manager Deployment not found")
}
