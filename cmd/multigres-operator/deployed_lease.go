package main

import (
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

const (
	leaderElectionID = "multigres-operator.multigres.com"

	// deployedOperatorNamespace is where the install manifests put the operator
	// (config/default/kustomization.yaml), and so where its leader-election
	// lease lives. It is not POD_NAMESPACE: an out-of-cluster run defaults that
	// to "default", where the deployed operator's lease never is, so looking
	// there would always conclude nothing is running.
	deployedOperatorNamespace = "multigres-operator"
)

// deployedOperatorConflict reports whether a cluster-wide run from outside the
// cluster could reconcile alongside a deployed operator. getErr is the result
// of reading that operator's leader-election lease.
//
// Existence is the whole test, not liveness. A cluster where an operator has
// ever been deployed has the lease until its namespace is deleted, so a bare
// Kind cluster is the only case that clears. Judging liveness instead would
// pass during every rollout, when the old leader has released the lease
// (LeaderElectionReleaseOnCancel) and the new one has not yet acquired it, and
// a laptop started in that window would run cluster-wide for its lifetime.
// Any read failure other than NotFound refuses: a laptop whose credentials
// cannot read the lease cannot tell a bare Kind cluster from a shared one.
func deployedOperatorConflict(getErr error) error {
	if apierrors.IsNotFound(getErr) {
		return nil
	}
	if getErr != nil {
		return fmt.Errorf(
			"cannot read lease %s/%s to tell whether an operator is deployed here "+
				"(use --namespace to run against one claimed namespace only): %w",
			deployedOperatorNamespace, leaderElectionID, getErr,
		)
	}
	return fmt.Errorf(
		"lease %s/%s exists, so an operator is or was deployed on this cluster and a "+
			"cluster-wide run could fight it over every object (use --namespace with a "+
			"namespace labelled multigres.com/reconcile=disabled, or delete the lease "+
			"if no operator is deployed)",
		deployedOperatorNamespace, leaderElectionID,
	)
}
