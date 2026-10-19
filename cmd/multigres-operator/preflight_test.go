package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestPreflight(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := coordinationv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	ns := func(name string, labels map[string]string) client.Object {
		return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
	}
	disabled := map[string]string{"multigres.com/reconcile": "disabled"}
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Namespace: deployedOperatorNamespace, Name: leaderElectionID},
	}

	tests := map[string]struct {
		objs          []client.Object
		laptopNS      string
		outOfCluster  bool
		getErr        error
		wantErrSubstr string
	}{
		"in cluster without --namespace checks nothing": {
			objs: []client.Object{lease},
		},
		"out of cluster on a bare cluster is allowed": {
			outOfCluster: true,
		},
		"out of cluster beside a deployed operator is refused": {
			objs:          []client.Object{lease},
			outOfCluster:  true,
			wantErrSubstr: "exists",
		},
		"a claimed namespace labelled disabled passes": {
			objs:     []client.Object{ns("a", disabled)},
			laptopNS: "a",
		},
		"the lease is not consulted when a namespace is claimed": {
			objs:         []client.Object{lease, ns("a", disabled)},
			laptopNS:     "a",
			outOfCluster: true,
		},
		"an unlabelled namespace is refused": {
			objs:          []client.Object{ns("a", nil)},
			laptopNS:      "a",
			wantErrSubstr: "not labelled",
		},
		"a malformed label value is refused": {
			objs: []client.Object{
				ns("a", map[string]string{"multigres.com/reconcile": "Disabled"}),
			},
			laptopNS:      "a",
			wantErrSubstr: "not labelled",
		},
		"a missing namespace is refused": {
			laptopNS:      "a",
			wantErrSubstr: "verifying",
		},
		"a namespace read error is refused": {
			laptopNS:      "a",
			getErr:        errors.New("connection refused"),
			wantErrSubstr: "connection refused",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			b := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tc.objs...)
			if tc.getErr != nil {
				b = b.WithInterceptorFuncs(interceptor.Funcs{
					Get: func(
						context.Context, client.WithWatch, client.ObjectKey, client.Object,
						...client.GetOption,
					) error {
						return tc.getErr
					},
				})
			}
			err := preflight(t.Context(), b.Build(), tc.laptopNS, tc.outOfCluster)
			if tc.wantErrSubstr == "" {
				if err != nil {
					t.Fatalf("preflight() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErrSubstr) {
				t.Fatalf("preflight() = %v, want an error mentioning %q", err, tc.wantErrSubstr)
			}
		})
	}
}
