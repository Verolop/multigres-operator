package toposerver

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	multigresv1alpha1 "github.com/multigres/multigres-operator/api/v1alpha1"
	"github.com/multigres/multigres-operator/pkg/util/certs"
)

func TestReconcileRepairsMaintenanceDependencies(t *testing.T) {
	for _, tc := range []struct {
		name       string
		age        time.Duration
		tls        bool
		unhealthy  bool
		wantActive bool
	}{
		{name: "operation still running", age: time.Minute, wantActive: true},
		{name: "TLS operation still running", age: time.Minute, tls: true, wantActive: true},
		{name: "member still unhealthy", age: 3 * time.Minute, unhealthy: true, wantActive: true},
		{name: "member recovered", age: 3 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, ts, etcd := maintenanceFixture(t)
			if err := policyv1.AddToScheme(r.Scheme); err != nil {
				t.Fatal(err)
			}
			key := client.ObjectKeyFromObject(ts)
			before := &appsv1.StatefulSet{}
			if err := r.Get(t.Context(), key, before); err != nil {
				t.Fatal(err)
			}
			if tc.tls {
				ts.Spec.TLS = &multigresv1alpha1.TopoTLSConfig{
					Enabled: ptr.To(true), IssuerName: "topology-issuer",
				}
				if err := r.Update(t.Context(), ts); err != nil {
					t.Fatal(err)
				}
				desired, err := BuildStatefulSet(ts, r.Scheme)
				if err != nil {
					t.Fatal(err)
				}
				before.Spec = desired.Spec
				if err := r.Update(t.Context(), before); err != nil {
					t.Fatal(err)
				}
			}
			ts.Status.EtcdMaintenance = &multigresv1alpha1.EtcdMaintenanceStatus{
				LastAttemptTime: metav1.NewTime(time.Now().Add(-tc.age)),
				Endpoint:        maintenanceEndpoints(ts)[0],
				InProgress:      true,
			}
			if err := r.Status().Update(t.Context(), ts); err != nil {
				t.Fatal(err)
			}
			ts.Spec.Etcd.Image = "etcd:pending-rollout"
			if err := r.Update(t.Context(), ts); err != nil {
				t.Fatal(err)
			}

			healthErr := errors.New("member is still unavailable")
			if tc.unhealthy {
				etcd.healthErr = healthErr
			}
			connectionAttempts := 0
			r.newMaintenanceClient = func(ctx context.Context, current *multigresv1alpha1.TopoServer) (etcdMaintenanceClient, error) {
				connectionAttempts++
				svc := &corev1.Service{}
				serviceKey := client.ObjectKey{
					Namespace: current.Namespace,
					Name:      current.Name + "-headless",
				}
				if err := r.APIReader.Get(ctx, serviceKey, svc); err != nil {
					return nil, fmt.Errorf("member DNS requires the headless Service: %w", err)
				}
				return etcd, nil
			}

			result, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key})
			if tc.unhealthy {
				if !errors.Is(err, healthErr) {
					t.Errorf("expected member health error, got %v", err)
				}
			} else if err != nil {
				t.Errorf("Reconcile() error = %v", err)
			}
			if tc.wantActive && result.RequeueAfter != statusRecheckDelay {
				t.Errorf("requeue = %v, want %v", result.RequeueAfter, statusRecheckDelay)
			}
			for _, name := range []string{ts.Name + "-headless", ts.Name} {
				svc := &corev1.Service{}
				if err := r.Get(
					t.Context(),
					client.ObjectKey{Namespace: ts.Namespace, Name: name},
					svc,
				); err != nil {
					t.Errorf("maintenance blocked Service repair: %v", err)
					continue
				}
				if !metav1.IsControlledBy(svc, ts) {
					t.Errorf("Service %s is not owned by the TopoServer", name)
				}
				if name == ts.Name+"-headless" &&
					(svc.Spec.ClusterIP != corev1.ClusterIPNone || !svc.Spec.PublishNotReadyAddresses) {
					t.Error("headless Service does not publish member DNS during recovery")
				}
			}
			if tc.tls {
				cert, err := certs.Get(
					t.Context(),
					r.Client,
					ts.Namespace,
					multigresv1alpha1.TopoServerCertName(ts.Name),
				)
				if err != nil || cert == nil {
					t.Errorf(
						"maintenance blocked Certificate repair: certificate=%v error=%v",
						cert,
						err,
					)
				}
			}

			fresh := &multigresv1alpha1.TopoServer{}
			if err := r.Get(t.Context(), key, fresh); err != nil {
				t.Fatal(err)
			}
			if fresh.Status.EtcdMaintenance == nil ||
				fresh.Status.EtcdMaintenance.InProgress != tc.wantActive {
				t.Errorf(
					"maintenance state = %+v, want active=%v",
					fresh.Status.EtcdMaintenance,
					tc.wantActive,
				)
			}
			after := &appsv1.StatefulSet{}
			if err := r.Get(t.Context(), key, after); err != nil {
				t.Fatal(err)
			}
			if tc.wantActive {
				if diff := cmp.Diff(before.Spec, after.Spec); diff != "" {
					t.Errorf("StatefulSet changed during maintenance (-before +after):\n%s", diff)
				}
			} else if after.Spec.Template.Spec.Containers[0].Image != string(ts.Spec.Etcd.Image) {
				t.Error("StatefulSet update did not resume after maintenance recovery")
			}
			if (connectionAttempts > 0) != (tc.age >= maintenanceTimeout) {
				t.Errorf("maintenance connection attempts = %d", connectionAttempts)
			}
			if len(etcd.defragged) != 0 {
				t.Error("recovery started another defragmentation")
			}
		})
	}
}
