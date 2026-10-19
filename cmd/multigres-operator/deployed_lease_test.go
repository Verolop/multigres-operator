package main

import (
	"errors"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestDeployedOperatorConflict(t *testing.T) {
	leaseGR := schema.GroupResource{Group: "coordination.k8s.io", Resource: "leases"}

	tests := map[string]struct {
		getErr        error
		wantErrSubstr string
	}{
		"no lease, as on a bare Kind cluster, allows the run": {
			getErr: apierrors.NewNotFound(leaseGR, leaderElectionID),
		},
		"an existing lease refuses, whoever holds it and however stale": {
			getErr:        nil,
			wantErrSubstr: "exists",
		},
		"a forbidden read refuses": {
			getErr:        apierrors.NewForbidden(leaseGR, leaderElectionID, errors.New("rbac")),
			wantErrSubstr: "cannot read lease",
		},
		"any other read error refuses": {
			getErr:        errors.New("connection refused"),
			wantErrSubstr: "cannot read lease",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			err := deployedOperatorConflict(tc.getErr)
			if tc.wantErrSubstr == "" {
				if err != nil {
					t.Fatalf("deployedOperatorConflict() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf(
					"deployedOperatorConflict() = nil, want an error mentioning %q",
					tc.wantErrSubstr,
				)
			}
			if !strings.Contains(err.Error(), tc.wantErrSubstr) {
				t.Fatalf(
					"deployedOperatorConflict() = %q, want it to mention %q",
					err,
					tc.wantErrSubstr,
				)
			}
		})
	}
}
