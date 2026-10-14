package shard

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	multigresv1alpha1 "github.com/multigres/multigres-operator/api/v1alpha1"
	"github.com/multigres/multigres-operator/pkg/data-handler/topo"
	"github.com/multigres/multigres-operator/pkg/monitoring"
	"github.com/multigres/multigres-operator/pkg/util/metadata"
)

const (
	// liteTopoTimeout bounds the topology write reconcileLite makes, matching
	// the deadline the data-plane phase puts on its topology connection.
	liteTopoTimeout = 30 * time.Second

	// litePrimaryRequeue is how often a Lite shard is reconciled while its
	// pooler does not report PRIMARY. The role is read over RPC, and a change
	// to it produces no Kubernetes event.
	litePrimaryRequeue = 10 * time.Second
)

// reconcileLite reconciles a Lite shard after the prelude every shard runs. A
// Lite shard is one pod running minigres and pgctld, plus its data PVC.
// minigres serves the cell's gateway and the shard's only pooler as a static
// leader. The rest of Reconcile does not run for it: there is no multiorch,
// PodDisruptionBudget, headless Service, rolling update, drain or data-plane
// phase. A pod whose spec drifts is deleted and created again.
func (r *ShardReconciler) reconcileLite(
	ctx context.Context,
	shard *multigresv1alpha1.Shard,
	rendered renderedConfig,
	storageClasses storageClassCheck,
) (ctrl.Result, error) {
	ctx, span := monitoring.StartChildSpan(ctx, "Shard.ReconcileLite")
	defer span.End()
	logger := log.FromContext(ctx)

	poolName, cellName, poolSpec, err := litePool(shard)
	if err != nil {
		monitoring.RecordSpanError(span, err)
		r.Recorder.Eventf(shard, "Warning", "ConfigError", "%v", err)
		return ctrl.Result{}, err
	}

	// The pod mounts the shared backup PVC when backups go to a filesystem,
	// and the rendered postgresql.conf, so both exist before the pod does.
	if err := r.reconcileSharedBackupPVC(ctx, shard); err != nil {
		monitoring.RecordSpanError(span, err)
		r.Recorder.Eventf(
			shard,
			"Warning",
			"FailedApply",
			"Failed to reconcile shared backup PVC: %v",
			err,
		)
		return ctrl.Result{}, err
	}
	if isMissingStorageClassDependency(storageClasses.poolDependency) {
		r.Recorder.Event(shard, "Warning", storageClassNotFoundReason, storageClasses.message)
		logger.Info(
			"StorageClass dependency missing for the Lite pod; requeueing",
			"after",
			storageClassDependencyRequeue,
		)
		return ctrl.Result{RequeueAfter: storageClassDependencyRequeue}, nil
	}
	if err := r.reconcilePostgresConfig(ctx, shard, rendered); err != nil {
		monitoring.RecordSpanError(span, err)
		r.Recorder.Eventf(
			shard,
			"Warning",
			"ConfigError",
			"Failed to reconcile postgres config: %v",
			err,
		)
		return ctrl.Result{}, err
	}

	// The pooler reads its backup location from the database record when it
	// bootstraps, so the record is registered before the pod is created.
	registered, err := r.registerLiteDatabase(ctx, shard)
	if err != nil {
		monitoring.RecordSpanError(span, err)
		return ctrl.Result{}, err
	}
	if !registered {
		return ctrl.Result{RequeueAfter: topoUnavailableRequeueDelay}, nil
	}

	if err := r.ensureLiteDataPVC(ctx, shard, poolName, cellName, poolSpec); err != nil {
		monitoring.RecordSpanError(span, err)
		r.Recorder.Eventf(shard, "Warning", "FailedApply", "Failed to reconcile data PVC: %v", err)
		return ctrl.Result{}, err
	}

	if err := r.ensureLitePod(ctx, shard, poolName, cellName, poolSpec); err != nil {
		monitoring.RecordSpanError(span, err)
		r.Recorder.Eventf(shard, "Warning", "FailedApply", "Failed to reconcile Lite pod: %v", err)
		return ctrl.Result{}, err
	}

	if !meta.IsStatusConditionTrue(shard.Status.Conditions, multigresv1alpha1.ConditionPrimary) {
		return ctrl.Result{RequeueAfter: litePrimaryRequeue}, nil
	}
	return ctrl.Result{}, nil
}

// litePool returns the pool and cell of a Lite shard. Admission allows exactly
// one pool with exactly one cell in Lite mode.
func litePool(
	shard *multigresv1alpha1.Shard,
) (poolName, cellName string, poolSpec multigresv1alpha1.PoolSpec, err error) {
	if len(shard.Spec.Pools) != 1 {
		return "", "", multigresv1alpha1.PoolSpec{}, fmt.Errorf(
			"lite shard %s has %d pools, want 1",
			shard.Name,
			len(shard.Spec.Pools),
		)
	}
	for name, pool := range shard.Spec.Pools {
		poolName, poolSpec = string(name), pool
	}
	if len(poolSpec.Cells) != 1 {
		return "", "", multigresv1alpha1.PoolSpec{}, fmt.Errorf(
			"lite pool %s has %d cells, want 1",
			poolName,
			len(poolSpec.Cells),
		)
	}
	return poolName, string(poolSpec.Cells[0]), poolSpec, nil
}

// registerLiteDatabase writes the shard's database record to the global
// topology with RegisterDatabase: its cells, the backup location from
// spec.backup and the durability policy. It reports false when the topology
// server cannot be reached yet, so the caller requeues as the data-plane phase
// does.
func (r *ShardReconciler) registerLiteDatabase(
	ctx context.Context,
	shard *multigresv1alpha1.Shard,
) (bool, error) {
	logger := log.FromContext(ctx)
	ctx, cancel := context.WithTimeout(ctx, liteTopoTimeout)
	defer cancel()

	store, err := r.topoStore(ctx, shard)
	if err != nil {
		if !topo.IsTopoUnavailable(err) {
			r.Recorder.Eventf(shard, "Warning", "TopologyError",
				"Failed to connect to topology store: %v", err)
		}
		logger.Error(err, "Failed to get topo store, cannot register database")
		return false, nil
	}
	defer func() { _ = store.Close() }()

	if err := topo.RegisterDatabase(ctx, store, r.Recorder, shard); err != nil {
		if topo.IsTopoUnavailable(err) {
			logger.Info("Topology server unavailable; requeueing", "error", err.Error())
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// ensureLiteDataPVC creates the Lite pod's data PVC when it is missing. It never
// deletes it: removal follows the PVC deletion policy when the Shard is
// deleted.
func (r *ShardReconciler) ensureLiteDataPVC(
	ctx context.Context,
	shard *multigresv1alpha1.Shard,
	poolName, cellName string,
	poolSpec multigresv1alpha1.PoolSpec,
) error {
	name := BuildPoolDataPVCName(shard, poolName, cellName, 0)
	err := r.Get(ctx, client.ObjectKey{Namespace: shard.Namespace, Name: name},
		&corev1.PersistentVolumeClaim{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to get PVC %s: %w", name, err)
	}

	pvc, err := BuildPoolDataPVC(
		shard,
		poolName,
		cellName,
		poolSpec,
		0,
		ShouldDeletePVCOnShardRemoval(shard, poolSpec),
		r.Scheme,
	)
	if err != nil {
		return fmt.Errorf("failed to build PVC %s: %w", name, err)
	}
	// Only one pod may use the data volume. ReadWriteOncePod has the kubelet
	// enforce that across nodes, so a replacement pod cannot start while the
	// old one still holds the volume.
	if len(poolSpec.Storage.AccessModes) == 0 {
		pvc.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod}
	}
	if err := r.Create(ctx, pvc); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create PVC %s: %w", name, err)
	}
	log.FromContext(ctx).Info("Created Lite data PVC", "pvc", name)
	return nil
}

// ensureLitePod creates the Lite pod when it is missing and deletes it when its
// spec-hash annotation no longer matches the desired pod, so the next reconcile
// creates it again. There is no disruption preflight and no drain: the pod is
// the shard's only pooler, so there is no cohort to protect.
func (r *ShardReconciler) ensureLitePod(
	ctx context.Context,
	shard *multigresv1alpha1.Shard,
	poolName, cellName string,
	poolSpec multigresv1alpha1.PoolSpec,
) error {
	logger := log.FromContext(ctx)

	desired, err := BuildLitePod(shard, poolName, cellName, poolSpec, r.Scheme)
	if err != nil {
		return fmt.Errorf("failed to build Lite pod: %w", err)
	}

	existing := &corev1.Pod{}
	err = r.Get(ctx, client.ObjectKeyFromObject(desired), existing)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, desired); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("failed to create pod %s: %w", desired.Name, err)
		}
		logger.Info("Created Lite pod", "pod", desired.Name)
		r.Recorder.Eventf(shard, "Normal", "PodCreated", "Created pod %s", desired.Name)
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to get pod %s: %w", desired.Name, err)
	}

	// A pod that is already terminating is replaced once it is gone; its
	// deletion requeues the shard.
	if !existing.DeletionTimestamp.IsZero() {
		return nil
	}
	if existing.Annotations[metadata.AnnotationSpecHash] ==
		desired.Annotations[metadata.AnnotationSpecHash] {
		return nil
	}

	logger.Info(
		"Deleting Lite pod to apply a spec change",
		"pod", existing.Name,
		"currentHash", existing.Annotations[metadata.AnnotationSpecHash],
		"desiredHash", desired.Annotations[metadata.AnnotationSpecHash],
	)
	if err := r.Delete(
		ctx,
		existing,
		client.Preconditions{UID: &existing.UID},
	); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete pod %s: %w", existing.Name, err)
	}
	r.Recorder.Eventf(
		shard,
		"Normal",
		"PodReplaced",
		"Deleted pod %s to apply a spec change",
		existing.Name,
	)
	return nil
}
