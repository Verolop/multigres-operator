package topology

import (
	"strings"
	"testing"

	"github.com/multigres/multigres-operator/pkg/util/metadata"
)

func TestRoots(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		annotations map[string]string
		namespace   string
		clusterName string
		cellName    string
		wantGlobal  string
		wantCell    string
		wantErr     bool
	}{
		"project reference is the stable identity": {
			annotations: map[string]string{metadata.AnnotationProjectRef: "proj_123"},
			namespace:   "ignored",
			clusterName: "ignored",
			cellName:    "eu-west-1",
			wantGlobal:  "/multigres/proj_123/global",
			wantCell:    "/multigres/proj_123/eu-west-1",
		},
		"namespace and name are the fallback identity": {
			namespace:   "customer-a",
			clusterName: "production",
			cellName:    "eu-west-1",
			wantGlobal:  "/multigres/customer-a/production/global",
			wantCell:    "/multigres/customer-a/production/eu-west-1",
		},
		"path separators and dot segments are encoded": {
			annotations: map[string]string{metadata.AnnotationProjectRef: "team/project"},
			cellName:    "..",
			wantGlobal:  "/multigres/team%2Fproject/global",
			wantCell:    "/multigres/team%2Fproject/%2E%2E",
		},
		"empty fallback namespace is rejected": {
			clusterName: "cluster",
			cellName:    "cell",
			wantErr:     true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			roots, err := NewRoots(tc.annotations, tc.namespace, tc.clusterName)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("NewRoots: %v", err)
			}
			cell, err := roots.Cell(tc.cellName)
			if err != nil {
				t.Fatalf("Cell: %v", err)
			}
			if got := roots.Global(); got != tc.wantGlobal {
				t.Errorf("Global() = %q, want %q", got, tc.wantGlobal)
			}
			if cell != tc.wantCell {
				t.Errorf("Cell() = %q, want %q", cell, tc.wantCell)
			}
			if roots.Global() == cell {
				t.Error("global and cell roots must be disjoint")
			}
		})
	}
}

func TestFallbackIdentityIsNamespaceScoped(t *testing.T) {
	t.Parallel()

	first, err := NewRoots(nil, "tenant-a", "production")
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewRoots(nil, "tenant-b", "production")
	if err != nil {
		t.Fatal(err)
	}
	if first.Global() == second.Global() {
		t.Fatalf("equal cluster names in different namespaces collided at %q", first.Global())
	}
}

func TestClusterRootPrefixesEveryRoot(t *testing.T) {
	tests := map[string]struct {
		annotations map[string]string
		namespace   string
		clusterName string
		want        string
	}{
		"project ref": {
			annotations: map[string]string{metadata.AnnotationProjectRef: "proj_123"},
			namespace:   "supabase",
			clusterName: "test-cluster",
			want:        "/multigres/proj_123",
		},
		"namespace and name fallback": {
			namespace:   "supabase",
			clusterName: "test-cluster",
			want:        "/multigres/supabase/test-cluster",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			roots, err := NewRoots(tc.annotations, tc.namespace, tc.clusterName)
			if err != nil {
				t.Fatalf("NewRoots() error = %v", err)
			}
			if got := roots.ClusterRoot(); got != tc.want {
				t.Errorf("ClusterRoot() = %q, want %q", got, tc.want)
			}
			if want := roots.ClusterRoot() + "/global"; roots.Global() != want {
				t.Errorf("Global() = %q, want %q", roots.Global(), want)
			}
			cell, err := roots.Cell("zone-a")
			if err != nil {
				t.Fatalf("Cell() error = %v", err)
			}
			if want := roots.ClusterRoot() + "/zone-a"; cell != want {
				t.Errorf("Cell() = %q, want %q", cell, want)
			}
		})
	}
}

// A range opened at the bare cluster root would reach a sibling whose identity
// merely starts with the same characters, so authorization grants on the
// prefix that includes the separator.
func TestKeyPrefixDoesNotReachSiblingClusters(t *testing.T) {
	short, err := NewRoots(
		map[string]string{metadata.AnnotationProjectRef: "proj_123"}, "supabase", "a",
	)
	if err != nil {
		t.Fatalf("NewRoots() error = %v", err)
	}
	long, err := NewRoots(
		map[string]string{metadata.AnnotationProjectRef: "proj_1234"}, "supabase", "b",
	)
	if err != nil {
		t.Fatalf("NewRoots() error = %v", err)
	}

	if !strings.HasPrefix(long.ClusterRoot(), short.ClusterRoot()) {
		t.Fatal("fixtures no longer exercise the sibling prefix hazard")
	}
	if strings.HasPrefix(long.Global(), short.KeyPrefix()) {
		t.Errorf(
			"KeyPrefix %q reaches sibling key %q",
			short.KeyPrefix(), long.Global(),
		)
	}
	if !strings.HasPrefix(short.Global(), short.KeyPrefix()) {
		t.Errorf("KeyPrefix %q does not cover its own keys", short.KeyPrefix())
	}
}
