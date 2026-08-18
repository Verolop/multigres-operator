// Package drain implements the drain state machine for graceful pod removal.
package drain

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	multigresv1alpha1 "github.com/multigres/multigres-operator/api/v1alpha1"
	"github.com/multigres/multigres-operator/pkg/monitoring"
	"github.com/multigres/multigres-operator/pkg/util/metadata"
)

const (
	// DrainTimeout is the maximum time a pod can remain in a drain state.
	DrainTimeout = 5 * time.Minute
)

// ExecuteDrainStateMachine advances a pod through its drain states.
func ExecuteDrainStateMachine(
	ctx context.Context,
	k8sClient client.Client,
	recorder record.EventRecorder,
	shard *multigresv1alpha1.Shard,
	pod *corev1.Pod,
) (bool, error) {
	logger := log.FromContext(ctx)

	state := pod.Annotations[metadata.AnnotationDrainState]
	if state == "" || state == metadata.DrainStateReadyForDeletion {
		return false, nil
	}

	clusterName := shard.Labels[metadata.LabelMultigresCluster]

	// Use AnnotationDrainRequestedAt if available, otherwise fall back to DeletionTimestamp.
	var drainStart time.Time
	if reqAtStr := pod.Annotations[metadata.AnnotationDrainRequestedAt]; reqAtStr != "" {
		if reqAt, err := time.Parse(time.RFC3339, reqAtStr); err == nil {
			drainStart = reqAt
		} else {
			logger.Error(
				err,
				"Malformed drain-requested-at annotation, using current time as fallback",
				"pod",
				pod.Name,
				"value",
				reqAtStr,
			)
			drainStart = time.Now()
		}
	}
	if drainStart.IsZero() && !pod.DeletionTimestamp.IsZero() {
		drainStart = pod.DeletionTimestamp.Time
	}

	if !drainStart.IsZero() && time.Since(drainStart) > DrainTimeout {
		logger.Info("Drain timed out; marking pod ready for deletion", "pod", pod.Name)
		return UpdateDrainState(ctx, k8sClient, pod, metadata.DrainStateReadyForDeletion)
	}

	switch state {
	case metadata.DrainStateRequested:
		logger.Info("Advancing drain state", "pod", pod.Name)
		return UpdateDrainState(ctx, k8sClient, pod, metadata.DrainStateDraining)

	case metadata.DrainStateDraining:
		return UpdateDrainState(ctx, k8sClient, pod, metadata.DrainStateAcknowledged)

	case metadata.DrainStateAcknowledged:
		monitoring.IncrementDrainOperations(clusterName, shard.Name, "success")
		if recorder != nil {
			recorder.Eventf(shard, "Normal", "DrainCompleted", "Pod %s is ready for deletion", pod.Name)
		}
		return UpdateDrainState(ctx, k8sClient, pod, metadata.DrainStateReadyForDeletion)
	}

	return false, nil
}

// UpdateDrainState patches a pod's drain state annotation.
func UpdateDrainState(
	ctx context.Context,
	k8sClient client.Client,
	pod *corev1.Pod,
	newState string,
) (bool, error) {
	patch := client.MergeFrom(pod.DeepCopy())
	if pod.Annotations == nil {
		pod.Annotations = make(map[string]string)
	}
	pod.Annotations[metadata.AnnotationDrainState] = newState
	if err := k8sClient.Patch(ctx, pod, patch); err != nil {
		return false, fmt.Errorf("updating pod drain state to %s: %w", newState, err)
	}
	return true, nil
}
