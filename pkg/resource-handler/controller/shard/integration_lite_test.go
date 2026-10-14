//go:build integration
// +build integration

package shard_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/multigres/multigres/go/common/topoclient"
	"github.com/multigres/multigres/go/common/topoclient/memorytopo"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"

	multigresv1alpha1 "github.com/multigres/multigres-operator/api/v1alpha1"
	shardcontroller "github.com/multigres/multigres-operator/pkg/resource-handler/controller/shard"
	"github.com/multigres/multigres-operator/pkg/testutil"
	"github.com/multigres/multigres-operator/pkg/util/metadata"
)

// newLiteTestShard returns a Lite shard with one pool in one cell and
// filesystem backups, as admission leaves it.
func newLiteTestShard(name, clusterName string) *multigresv1alpha1.Shard {
	return &multigresv1alpha1.Shard{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels:    map[string]string{metadata.LabelMultigresCluster: clusterName},
		},
		Spec: multigresv1alpha1.ShardSpec{
			Mode:           multigresv1alpha1.ModeLite,
			DatabaseName:   "testdb",
			TableGroupName: "default",
			ShardName:      "0-inf",
			LogLevels: multigresv1alpha1.ComponentLogLevels{
				Pgctld:       "info",
				Multipooler:  "info",
				Multiorch:    "info",
				Multiadmin:   "info",
				Multigateway: "info",
			},
			Multiorch: multigresv1alpha1.MultiorchSpec{
				Cells: []multigresv1alpha1.CellName{"zone1"},
			},
			Images: multigresv1alpha1.ShardImages{
				Multiorch:   "ghcr.io/multigres/multigres:main",
				Multipooler: "ghcr.io/multigres/multigres:main",
				Postgres:    "postgres:17",
			},
			GlobalTopoServer: multigresv1alpha1.GlobalTopoServerRef{
				Address: "global-topo:2379", RootPath: "/multigres/global", Implementation: "etcd",
			},
			Pools: map[multigresv1alpha1.PoolName]multigresv1alpha1.PoolSpec{
				"default": {
					Cells:           []multigresv1alpha1.CellName{"zone1"},
					Type:            "readWrite",
					ReplicasPerCell: ptr.To(int32(1)),
					Storage:         multigresv1alpha1.StorageSpec{Size: "1Gi"},
				},
			},
			Backup: &multigresv1alpha1.BackupConfig{
				Type: multigresv1alpha1.BackupTypeFilesystem,
				Filesystem: &multigresv1alpha1.FilesystemBackupConfig{
					Path:    "/backups",
					Storage: multigresv1alpha1.StorageSpec{Size: "1Gi"},
				},
			},
		},
	}
}

// liteTopo returns an in-memory topology and a store factory for the shard
// controller, so the test can read the records the controller writes.
func liteTopo(t *testing.T) (topoclient.Store, func(*multigresv1alpha1.Shard) (topoclient.Store, error)) {
	t.Helper()
	store, factory := memorytopo.NewServerAndFactory(t.Context(), "zone1")
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return store, func(*multigresv1alpha1.Shard) (topoclient.Store, error) {
		return topoclient.NewWithFactory(
			factory,
			"",
			[]string{""},
			topoclient.NewDefaultTopoConfig(),
		), nil
	}
}

func TestReconcileLite(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	_ = multigresv1alpha1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = policyv1.AddToScheme(scheme)

	mgr := testutil.SetUpEnvtestManager(t, scheme,
		testutil.WithCRDPaths(filepath.Join("../../../../", "config", "crd", "bases")),
	)
	ctx := t.Context()
	k8sClient := testutil.SetUpClient(t, mgr.GetConfig(), scheme)

	const clusterName = "lite-cluster"
	shard := newLiteTestShard("lite-shard", clusterName)

	store, topoFactory := liteTopo(t)
	if err := (&shardcontroller.ShardReconciler{
		Client:          mgr.GetClient(),
		APIReader:       mgr.GetAPIReader(),
		Scheme:          mgr.GetScheme(),
		Recorder:        mgr.GetEventRecorderFor("shard-controller"),
		CreateTopoStore: topoFactory,
	}).SetupWithManager(mgr, controller.Options{SkipNameValidation: ptr.To(true)}); err != nil {
		t.Fatalf("Failed to create controller: %v", err)
	}

	setTestPostgresPasswordSecretRef(shard)
	createTestPostgresPasswordSecret(t, ctx, k8sClient, shard.Namespace)
	require.NoError(t, k8sClient.Create(ctx, shard))

	podKey := types.NamespacedName{
		Namespace: "default",
		Name:      shardcontroller.BuildPoolPodName(shard, "default", "zone1", 0),
	}
	pvcKey := types.NamespacedName{
		Namespace: "default",
		Name:      shardcontroller.BuildPoolDataPVCName(shard, "default", "zone1", 0),
	}

	pod := &corev1.Pod{}
	require.Eventually(t, func() bool {
		return k8sClient.Get(ctx, podKey, pod) == nil
	}, 30*time.Second, 200*time.Millisecond, "Lite pod was not created")

	// The shard registered its database before creating the pod, with the
	// cells, backup location and durability policy from its spec.
	db, err := store.GetDatabase(ctx, "testdb")
	require.NoError(t, err, "database was not registered in topology")
	require.Equal(t, []string{"zone1"}, db.GetCells())
	require.Equal(t, "/backups", db.GetBackupLocation().GetFilesystem().GetPath())
	require.Equal(t, "AT_LEAST_2", db.GetBootstrapDurabilityPolicy().GetPolicyName())
	require.Equal(t, metadata.GatewayRoleLite, pod.Labels[metadata.LabelMultigresGatewayRole])
	require.Empty(t, pod.Spec.ReadinessGates)
	require.Equal(t, "minigres", pod.Spec.Containers[0].Name)

	pvc := &corev1.PersistentVolumeClaim{}
	require.NoError(t, k8sClient.Get(ctx, pvcKey, pvc))
	require.Equal(t,
		[]corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod},
		pvc.Spec.AccessModes,
	)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{
		Namespace: "default",
		Name:      shardcontroller.BuildSharedBackupPVCName(shard),
	}, &corev1.PersistentVolumeClaim{}), "shared backup PVC the pod mounts")

	// The non-Lite path creates these before any pool pod, so their absence
	// once the pod exists shows that path did not run.
	byCluster := client.MatchingLabels{metadata.LabelMultigresCluster: clusterName}
	pdbs := &policyv1.PodDisruptionBudgetList{}
	require.NoError(t, k8sClient.List(ctx, pdbs, client.InNamespace("default"), byCluster))
	require.Empty(t, pdbs.Items, "Lite shard must not have a PodDisruptionBudget")
	deployments := &appsv1.DeploymentList{}
	require.NoError(t, k8sClient.List(ctx, deployments, client.InNamespace("default"), byCluster))
	require.Empty(t, deployments.Items, "Lite shard must not have a multiorch Deployment")
	services := &corev1.ServiceList{}
	require.NoError(t, k8sClient.List(ctx, services, client.InNamespace("default"), byCluster))
	require.Empty(t, services.Items, "Lite shard must not have multiorch or headless Services")

	// A spec change that moves the spec-hash replaces the pod.
	originalUID := pod.UID
	originalHash := pod.Annotations[metadata.AnnotationSpecHash]
	require.NoError(t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &multigresv1alpha1.Shard{}
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(shard), current); err != nil {
			return err
		}
		base := current.DeepCopy()
		current.Spec.Images.Multipooler = "ghcr.io/multigres/multigres:next"
		return k8sClient.Patch(ctx, current, client.MergeFrom(base))
	}))

	require.Eventually(t, func() bool {
		replaced := &corev1.Pod{}
		if err := k8sClient.Get(ctx, podKey, replaced); err != nil {
			return false
		}
		return replaced.UID != originalUID &&
			replaced.Annotations[metadata.AnnotationSpecHash] != originalHash &&
			replaced.Spec.Containers[0].Image == "ghcr.io/multigres/multigres:next"
	}, 30*time.Second, 200*time.Millisecond, "Lite pod was not replaced after a spec change")

	// The data PVC survives the replacement.
	replacedPVC := &corev1.PersistentVolumeClaim{}
	require.NoError(t, k8sClient.Get(ctx, pvcKey, replacedPVC))
	require.Equal(t, pvc.UID, replacedPVC.UID)

	// Deletion runs the shared handleDeletion path, which removes the pod and
	// then the finalizer without a multiorch or PDB to wait for.
	require.NoError(t, k8sClient.Delete(ctx, shard))
	require.Eventually(t, func() bool {
		return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(shard),
			&multigresv1alpha1.Shard{})) &&
			apierrors.IsNotFound(k8sClient.Get(ctx, podKey, &corev1.Pod{}))
	}, 30*time.Second, 200*time.Millisecond, "Lite shard and pod were not deleted")
}
