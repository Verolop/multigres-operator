//go:build e2e

// Package framework provides reusable infrastructure for e2e tests.
//
// Two cluster modes are supported:
//
//   - Shared: one Kind cluster reused across test packages (fast, namespace isolation).
//     Use [EnsureSharedCluster] in each package's TestMain.
//   - Dedicated: one Kind cluster per test package (full isolation).
//     Use [NewDedicatedCluster] in the package's TestMain.
package framework

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	kind "sigs.k8s.io/e2e-framework/third_party/kind"

	multigresv1alpha1 "github.com/multigres/multigres-operator/api/v1alpha1"
	"github.com/multigres/multigres-operator/pkg/testutil"
)

// SharedClusterName is the well-known name for the shared Kind cluster.
const SharedClusterName = "e2e-shared"

// Cluster holds connection state for a Kind cluster.
type Cluster struct {
	Name       string
	Kubeconfig string
	RestCfg    *rest.Config
	Clientset  *kubernetes.Clientset
	provider   *kind.Cluster
}

// EnsureSharedCluster creates the shared Kind cluster if it doesn't exist,
// loads images, and deploys the operator. Idempotent — safe to call from
// multiple TestMain functions; existing clusters are reused.
func EnsureSharedCluster() (*Cluster, error) {
	return setupCluster(SharedClusterName)
}

// NewDedicatedCluster creates a new dedicated Kind cluster with the given name.
func NewDedicatedCluster(name string) (*Cluster, error) {
	return setupCluster(name)
}

// Destroy tears down the Kind cluster and removes the kubeconfig.
func (c *Cluster) Destroy() {
	if err := c.provider.Destroy(context.Background()); err != nil {
		logf("warning: destroy cluster %q: %v", c.Name, err)
	}
}

// ShouldKeepCluster returns true if E2E_KEEP_CLUSTERS says to keep it.
func ShouldKeepCluster(failed bool) bool {
	keep := os.Getenv("E2E_KEEP_CLUSTERS")
	return strings.EqualFold(keep, "always") ||
		(strings.EqualFold(keep, "on-failure") && failed)
}

// CRClient returns a controller-runtime client with the multigres scheme.
func (c *Cluster) CRClient() (client.Client, error) {
	scheme := runtime.NewScheme()
	_ = multigresv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	_ = policyv1.AddToScheme(scheme)
	return client.New(c.RestCfg, client.Options{Scheme: scheme})
}

// LoadImages loads locally tagged container images into every Kind node.
func LoadImages(ctx context.Context, clusterName string, images []string) error {
	images = kindLoadableImages(images)
	if len(images) == 0 {
		return nil
	}
	nodes, err := kindNodes(ctx, clusterName)
	if err != nil {
		return err
	}
	logf("loading %d locally tagged images into %s...", len(images), clusterName)
	for _, image := range images {
		for _, node := range nodes {
			if err := importImage(ctx, node, image); err != nil {
				return fmt.Errorf("load image %q into node %q: %w", image, node, err)
			}
		}
	}
	return nil
}

func kindNodes(ctx context.Context, clusterName string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "kind", "get", "nodes", "--name", clusterName)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("get nodes for Kind cluster %q: %w", clusterName, err)
	}
	nodes := strings.Fields(string(out))
	if len(nodes) == 0 {
		return nil, fmt.Errorf("Kind cluster %q has no nodes", clusterName)
	}
	return nodes, nil
}

func importImage(ctx context.Context, node, image string) error {
	save := exec.CommandContext(ctx, "docker", "save", image)
	ctrImport := exec.CommandContext(ctx, "docker", "exec", "-i", node,
		"ctr", "--namespace=k8s.io", "images", "import", "-")

	archive, err := save.StdoutPipe()
	if err != nil {
		return err
	}
	ctrImport.Stdin = archive
	ctrImport.Stdout = os.Stderr
	ctrImport.Stderr = os.Stderr
	if err := ctrImport.Start(); err != nil {
		return err
	}
	if err := save.Run(); err != nil {
		_ = ctrImport.Wait()
		return fmt.Errorf("save Docker image: %w", err)
	}
	if err := ctrImport.Wait(); err != nil {
		return fmt.Errorf("import image into containerd: %w", err)
	}
	return nil
}

func kindLoadableImages(images []string) []string {
	loadable := make([]string, 0, len(images))
	for _, image := range images {
		if strings.Contains(image, "@") {
			continue
		}
		loadable = append(loadable, image)
	}
	return loadable
}

// ---------------------------------------------------------------------------
// Internal
// ---------------------------------------------------------------------------

func setupCluster(name string) (*Cluster, error) {
	ctx := context.Background()

	// Serialize cluster setup across test binaries. go test runs each
	// package as a separate process; without locking they all race to
	// create the same Kind cluster and only one wins.
	unlock, err := lockSetup(name)
	if err != nil {
		return nil, err
	}
	defer unlock()

	// Create or connect (idempotent — guarded by file lock above).
	p := kind.NewCluster(name)
	kubecfg, err := p.Create(ctx)
	if err != nil {
		return nil, fmt.Errorf("create cluster %q: %w", name, err)
	}

	restCfg := p.KubernetesRestConfig()
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("create clientset: %w", err)
	}

	c := &Cluster{
		Name:       name,
		Kubeconfig: kubecfg,
		RestCfg:    restCfg,
		Clientset:  cs,
		provider:   p,
	}

	// Label nodes with cloud topology labels so pods with nodeSelectors
	// derived from zoneId/region fields can be scheduled in Kind.
	if err := labelAllNodes(kubecfg, map[string]string{
		"topology.k8s.aws/zone-id":      "us-central1-a",
		"topology.kubernetes.io/region": "us-central1",
	}); err != nil {
		return nil, fmt.Errorf("label kind nodes: %w", err)
	}

	// Load images directly into each Kind node; idempotent.
	preset := testutil.DefaultOperatorPreset()
	imgs := append([]string{preset.Image}, runtimeImages()...)
	if err := LoadImages(ctx, name, imgs); err != nil {
		return nil, fmt.Errorf("load images: %w", err)
	}

	// Deploy operator (idempotent — server-side apply + rollout status).
	if err := deployOperator(kubecfg, preset.Image, preset.Namespace, preset.DeploymentName); err != nil {
		return nil, fmt.Errorf("deploy operator: %w", err)
	}

	return c, nil
}

func deployOperator(kubecfg, image, namespace, deploymentName string) error {
	repoRoot, err := repoRoot()
	if err != nil {
		return err
	}
	kustomize, err := kustomizeBinary(repoRoot)
	if err != nil {
		return err
	}

	// 1. Install CRDs.
	logf("installing CRDs...")
	if err := kustomizePipe(kustomize, kubecfg, filepath.Join(repoRoot, "config", "crd")); err != nil {
		return fmt.Errorf("install CRDs: %w", err)
	}

	// 2. Copy config/ to temp, set operator image, apply no-webhook overlay.
	tmpDir, err := os.MkdirTemp("", "e2e-config-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	cpCmd := exec.Command("cp", "-r", filepath.Join(repoRoot, "config"), tmpDir)
	if out, err := cpCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("copy config: %w\n%s", err, out)
	}

	setImg := exec.Command(kustomize, "edit", "set", "image", fmt.Sprintf("controller=%s", image))
	setImg.Dir = filepath.Join(tmpDir, "config", "manager")
	if out, err := setImg.CombinedOutput(); err != nil {
		return fmt.Errorf("kustomize edit set image: %w\n%s", err, out)
	}

	logf("deploying operator (image=%s)...", image)
	if err := kustomizePipe(kustomize, kubecfg, filepath.Join(tmpDir, "config", "no-webhook")); err != nil {
		return fmt.Errorf("apply operator: %w", err)
	}

	// 3. Wait for rollout.
	logf("waiting for operator rollout...")
	cmd := exec.Command("kubectl", "--kubeconfig", kubecfg,
		"-n", namespace, "rollout", "status", "deployment/"+deploymentName, "--timeout=180s")
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("operator rollout: %w", err)
	}
	logf("operator ready ✓")
	return nil
}

func kustomizeBinary(repoRoot string) (string, error) {
	local := filepath.Join(repoRoot, "bin", "kustomize")
	if info, err := os.Stat(local); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
		return local, nil
	}
	path, err := exec.LookPath("kustomize")
	if err != nil {
		return "", fmt.Errorf("find kustomize: install it with make kustomize: %w", err)
	}
	return path, nil
}

func kustomizePipe(kustomize, kubecfg, dir string) error {
	kBuild := exec.Command(kustomize, "build", dir)
	kApply := exec.Command("kubectl", "--kubeconfig", kubecfg, "apply", "--server-side", "-f", "-")
	pipe, err := kBuild.StdoutPipe()
	if err != nil {
		return err
	}
	kApply.Stdin = pipe
	kApply.Stdout = os.Stderr
	kApply.Stderr = os.Stderr
	kBuild.Stderr = os.Stderr
	if err := kApply.Start(); err != nil {
		return err
	}
	if err := kBuild.Run(); err != nil {
		return fmt.Errorf("kustomize build %s: %w", dir, err)
	}
	return kApply.Wait()
}

func repoRoot() (string, error) {
	// Prefer REPO_ROOT env var (set by Makefile / CI).
	if r := os.Getenv("REPO_ROOT"); r != "" {
		return r, nil
	}
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// lockSetup acquires an exclusive file lock to serialize cluster setup
// across concurrent test binaries.
func lockSetup(name string) (unlock func(), err error) {
	lockPath := filepath.Join(os.TempDir(), fmt.Sprintf("e2e-%s.lock", name))
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("acquire setup lock: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// labelAllNodes applies the given labels to all nodes in the cluster.
// Uses --overwrite so repeated calls are idempotent.
func labelAllNodes(kubecfg string, labels map[string]string) error {
	for k, v := range labels {
		label := fmt.Sprintf("%s=%s", k, v)
		cmd := exec.Command("kubectl", "--kubeconfig", kubecfg,
			"label", "nodes", "--all", "--overwrite", label)
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("label nodes %s: %w", label, err)
		}
	}
	return nil
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "e2e: "+format+"\n", args...)
}
