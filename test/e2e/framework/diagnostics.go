//go:build e2e

package framework

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	diagnosticTimeout      = 2 * time.Minute
	diagnosticDirectoryEnv = "E2E_FAILURE_LOG_DIR"
)

// dumpNamespaceDiagnostics captures failure evidence before namespace cleanup.
// kubectl cluster-info dump collects standard workload state, events, and pod
// logs, but does not enumerate Secret objects.
func (c *Cluster) dumpNamespaceDiagnostics(t testing.TB, ns string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), diagnosticTimeout)
	defer cancel()

	root := os.Getenv(diagnosticDirectoryEnv)
	if root == "" {
		root = filepath.Join(os.TempDir(), "e2e-failure-logs")
	}
	dir := filepath.Join(root, safeDiagnosticName(ns))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Logf("create diagnostics directory for namespace %s: %v", ns, err)
		return
	}
	c.dumpMultigresResourceState(ctx, t, ns, dir)

	cmd := exec.CommandContext(
		ctx,
		"kubectl",
		"--kubeconfig", c.Kubeconfig,
		"cluster-info", "dump",
		"--namespaces", ns,
		"--output-directory", dir,
		"--output", "yaml",
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf(
			"collect diagnostics for namespace %s: %v: %s",
			ns, err, strings.TrimSpace(string(output)),
		)
		return
	}
	t.Logf("captured failure diagnostics for namespace %s in %s", ns, dir)
}

func (c *Cluster) dumpMultigresResourceState(
	ctx context.Context,
	t testing.TB,
	ns, dir string,
) {
	t.Helper()
	cmd := exec.CommandContext(
		ctx,
		"kubectl",
		"--kubeconfig", c.Kubeconfig,
		"get",
		"multigresclusters.multigres.com,"+
			"cells.multigres.com,"+
			"shards.multigres.com,"+
			"toposervers.multigres.com",
		"--namespace", ns,
		"--output", "json",
	)
	output, err := cmd.Output()
	if err != nil {
		detail := ""
		if exitErr, ok := err.(*exec.ExitError); ok {
			detail = strings.TrimSpace(string(exitErr.Stderr))
		}
		t.Logf("collect Multigres resource state for namespace %s: %v: %s", ns, err, detail)
		return
	}
	output, err = resourceStatusOutput(output)
	if err != nil {
		t.Logf("sanitize Multigres resource state for namespace %s: %v", ns, err)
		return
	}
	path := filepath.Join(dir, "multigres-resource-status.json")
	if err := os.WriteFile(path, append(output, '\n'), 0o600); err != nil {
		t.Logf("write Multigres resource state for namespace %s: %v", ns, err)
	}
}

type diagnosticResourceList struct {
	Items []diagnosticResource `json:"items"`
}

type diagnosticResource struct {
	APIVersion string                     `json:"apiVersion"`
	Kind       string                     `json:"kind"`
	Metadata   diagnosticResourceMetadata `json:"metadata"`
	Status     json.RawMessage            `json:"status,omitempty"`
}

type diagnosticResourceMetadata struct {
	Name       string `json:"name"`
	Namespace  string `json:"namespace"`
	Generation int64  `json:"generation"`
}

func resourceStatusOutput(input []byte) ([]byte, error) {
	resources := diagnosticResourceList{}
	if err := json.Unmarshal(input, &resources); err != nil {
		return nil, err
	}
	return json.MarshalIndent(resources, "", "  ")
}

func safeDiagnosticName(value string) string {
	name := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '.' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, value)
	name = strings.Trim(name, ".")
	if name == "" {
		return "unnamed"
	}
	return name
}
