package main

import (
	"context"
	"errors"
	"fmt"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/multigres/multigres-operator/pkg/features"
)

// outOfCluster is untagged on purpose: a stock build run from a laptop must hit
// the deployed-operator lease check as much as a kubedial build does.
func outOfCluster() bool {
	_, err := rest.InClusterConfig()
	return errors.Is(err, rest.ErrNotInCluster)
}

// preflight refuses configurations that would put this process on objects a
// deployed operator is also reconciling. Without --namespace, an out-of-cluster
// run is checked against the deployed operator's lease; with it, the claimed
// namespace must exist and already carry the label that keeps the deployed
// operator out, or the two fight over the same objects (the applied-images
// oscillation). The Get is also what rejects a malformed namespace name.
func preflight(
	ctx context.Context,
	r client.Reader,
	laptopNS string,
	outOfCluster bool,
) error {
	if laptopNS == "" {
		if !outOfCluster {
			return nil
		}
		var lease coordinationv1.Lease
		getErr := r.Get(
			ctx,
			client.ObjectKey{Namespace: deployedOperatorNamespace, Name: leaderElectionID},
			&lease,
		)
		return deployedOperatorConflict(getErr)
	}
	var ns corev1.Namespace
	if err := r.Get(ctx, client.ObjectKey{Name: laptopNS}, &ns); err != nil {
		return fmt.Errorf("verifying --namespace %q: %w", laptopNS, err)
	}
	if ns.Labels[features.ReconcileLabel] != features.Disabled {
		return fmt.Errorf(
			"namespace %q is not labelled %s=%s: label it first, or the deployed "+
				"operator will fight this process over it",
			laptopNS, features.ReconcileLabel, features.Disabled,
		)
	}
	return nil
}
