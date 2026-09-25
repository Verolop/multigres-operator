package toposerver

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	multigresv1alpha1 "github.com/multigres/multigres-operator/api/v1alpha1"
)

type fakeEtcdMaintenance struct {
	statuses    map[string]*clientv3.StatusResponse
	defragged   []string
	moved       []uint64
	healthErr   error
	defragErr   error
	afterDefrag func()
}

func (f *fakeEtcdMaintenance) Status(
	_ context.Context,
	ep string,
) (*clientv3.StatusResponse, error) {
	return f.statuses[ep], nil
}

func (f *fakeEtcdMaintenance) Members(context.Context) (*clientv3.MemberListResponse, error) {
	return &clientv3.MemberListResponse{Members: []*pb.Member{{ID: 1}, {ID: 2}, {ID: 3}}}, nil
}
func (f *fakeEtcdMaintenance) Health(context.Context, string) error { return f.healthErr }
func (f *fakeEtcdMaintenance) Defragment(_ context.Context, ep string) error {
	f.defragged = append(f.defragged, ep)
	if f.afterDefrag != nil {
		f.afterDefrag()
	}
	return f.defragErr
}

func (f *fakeEtcdMaintenance) MoveLeader(_ context.Context, _ string, id uint64) error {
	f.moved = append(f.moved, id)
	for _, s := range f.statuses {
		s.Leader = id
	}
	return nil
}
func (f *fakeEtcdMaintenance) Close() {}

func maintenanceFixture(
	t *testing.T,
) (*TopoServerReconciler, *multigresv1alpha1.TopoServer, *fakeEtcdMaintenance) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = multigresv1alpha1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	ts := certTestTopoServer(nil)
	ts.Generation = 1
	ts.Spec.Etcd.Maintenance = &multigresv1alpha1.EtcdMaintenanceConfig{
		DefragmentationEnabled: ptr.To(true),
	}
	sts, err := BuildStatefulSet(ts, scheme)
	if err != nil {
		t.Fatal(err)
	}
	sts.UID, sts.Generation = "sts-uid", 1
	sts.Status = appsv1.StatefulSetStatus{
		ObservedGeneration: 1,
		Replicas:           3,
		ReadyReplicas:      3,
		UpdatedReplicas:    3,
		CurrentRevision:    "rev1",
		UpdateRevision:     "rev1",
	}
	objects := []client.Object{ts, sts}
	f := &fakeEtcdMaintenance{statuses: map[string]*clientv3.StatusResponse{}}
	for i, ep := range maintenanceEndpoints(ts) {
		f.statuses[ep] = &clientv3.StatusResponse{
			Header:      &pb.ResponseHeader{ClusterId: 123, MemberId: uint64(i + 1)},
			Leader:      1,
			DbSize:      400 << 20,
			DbSizeInUse: 100 << 20,
		}
		objects = append(objects, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("%s-%d", ts.Name, i),
				Namespace: ts.Namespace,
				Labels:    map[string]string{appsv1.StatefulSetRevisionLabel: "rev1"},
				OwnerReferences: []metav1.OwnerReference{
					*metav1.NewControllerRef(sts, appsv1.SchemeGroupVersion.WithKind("StatefulSet")),
				},
			},
			Status: corev1.PodStatus{
				Conditions: []corev1.PodCondition{
					{
						Type:               corev1.PodReady,
						Status:             corev1.ConditionTrue,
						LastTransitionTime: metav1.NewTime(time.Now().Add(-time.Hour)),
					},
				},
			},
		})
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&multigresv1alpha1.TopoServer{}, &appsv1.StatefulSet{}, &corev1.Pod{}).
		WithObjects(objects...).
		Build()
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(ts), ts); err != nil {
		t.Fatal(err)
	}
	r := &TopoServerReconciler{
		Client:               c,
		APIReader:            c,
		Scheme:               scheme,
		Recorder:             record.NewFakeRecorder(100),
		newMaintenanceClient: func(context.Context, *multigresv1alpha1.TopoServer) (etcdMaintenanceClient, error) { return f, nil },
	}
	return r, ts, f
}

func TestEtcdMaintenanceSerializesMembersAndRestarts(t *testing.T) {
	r, ts, f := maintenanceFixture(t)
	if err := r.reconcileMaintenance(t.Context(), ts); err != nil {
		t.Fatal(err)
	}
	if len(f.defragged) != 1 || len(f.moved) != 1 {
		t.Fatalf("defrags=%v leader transfers=%v", f.defragged, f.moved)
	}
	fresh := &multigresv1alpha1.TopoServer{}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(ts), fresh); err != nil {
		t.Fatal(err)
	}
	if fresh.Status.EtcdMaintenance == nil || fresh.Status.EtcdMaintenance.InProgress {
		t.Fatal("completed reservation not persisted")
	}
	// A new controller instance, or a stale reconcile, must obey the persisted interval.
	r2 := *r
	if err := r2.reconcileMaintenance(t.Context(), fresh); err != nil {
		t.Fatal(err)
	}
	if len(f.defragged) != 1 {
		t.Fatal("maintenance repeated inside the interval")
	}
}

func TestEtcdMaintenanceHealthGates(t *testing.T) {
	for _, test := range []struct {
		name    string
		mutate  func(*TopoServerReconciler, *multigresv1alpha1.TopoServer, *fakeEtcdMaintenance)
		wantErr bool
	}{
		{"disabled", func(_ *TopoServerReconciler, ts *multigresv1alpha1.TopoServer, _ *fakeEtcdMaintenance) {
			ts.Spec.Etcd.Maintenance = nil
		}, false},
		{"single member", func(_ *TopoServerReconciler, ts *multigresv1alpha1.TopoServer, _ *fakeEtcdMaintenance) {
			ts.Spec.Etcd.Replicas = ptr.To(int32(1))
		}, false},
		{"no fragmentation", func(_ *TopoServerReconciler, _ *multigresv1alpha1.TopoServer, f *fakeEtcdMaintenance) {
			for _, s := range f.statuses {
				s.DbSizeInUse = s.DbSize
			}
		}, false},
		{"small fragmentation", func(_ *TopoServerReconciler, _ *multigresv1alpha1.TopoServer, f *fakeEtcdMaintenance) {
			for _, s := range f.statuses {
				s.DbSize = 100 << 20
				s.DbSizeInUse = 10 << 20
			}
		}, false},
		{"low fragmentation ratio", func(_ *TopoServerReconciler, _ *multigresv1alpha1.TopoServer, f *fakeEtcdMaintenance) {
			for _, s := range f.statuses {
				s.DbSize = 1 << 30
				s.DbSizeInUse = 900 << 20
			}
		}, false},
		{"linearizable read fails", func(_ *TopoServerReconciler, _ *multigresv1alpha1.TopoServer, f *fakeEtcdMaintenance) {
			f.healthErr = errors.New("no quorum")
		}, true},
		{"leader disagreement", func(_ *TopoServerReconciler, ts *multigresv1alpha1.TopoServer, f *fakeEtcdMaintenance) {
			f.statuses[maintenanceEndpoints(ts)[1]].Leader = 2
		}, true},
		{"member alarm", func(_ *TopoServerReconciler, ts *multigresv1alpha1.TopoServer, f *fakeEtcdMaintenance) {
			f.statuses[maintenanceEndpoints(ts)[1]].Errors = []string{"NOSPACE"}
		}, true},
		{"foreign cluster", func(_ *TopoServerReconciler, ts *multigresv1alpha1.TopoServer, f *fakeEtcdMaintenance) {
			f.statuses[maintenanceEndpoints(ts)[1]].Header.ClusterId = 999
		}, true},
		{"duplicate member", func(_ *TopoServerReconciler, ts *multigresv1alpha1.TopoServer, f *fakeEtcdMaintenance) {
			f.statuses[maintenanceEndpoints(ts)[1]].Header.MemberId = 1
		}, true},
		{"rolling update", func(r *TopoServerReconciler, ts *multigresv1alpha1.TopoServer, _ *fakeEtcdMaintenance) {
			sts := &appsv1.StatefulSet{}
			_ = r.Get(t.Context(), client.ObjectKeyFromObject(ts), sts)
			sts.Status.UpdateRevision = "rev2"
			if err := r.Status().Update(t.Context(), sts); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"reservation conflict", func(r *TopoServerReconciler, _ *multigresv1alpha1.TopoServer, _ *fakeEtcdMaintenance) {
			r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{SubResourcePatch: func(context.Context, client.Client, string, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
				return apierrors.NewConflict(multigresv1alpha1.GroupVersion.WithResource("toposervers").GroupResource(), "topo", errors.New("conflict"))
			}})
		}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			r, ts, f := maintenanceFixture(t)
			test.mutate(r, ts, f)
			err := r.reconcileMaintenance(t.Context(), ts)
			if (err != nil) != test.wantErr {
				t.Fatalf("error=%v", err)
			}
			if len(f.defragged) != 0 {
				t.Fatalf("unsafe defragmentation: %v", f.defragged)
			}
		})
	}
}

func TestEtcdMaintenanceInterruptedOperation(t *testing.T) {
	r, ts, f := maintenanceFixture(t)
	f.defragErr = context.DeadlineExceeded
	if err := r.reconcileMaintenance(t.Context(), ts); err == nil {
		t.Fatal("expected timeout")
	}
	fresh := &multigresv1alpha1.TopoServer{}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(ts), fresh); err != nil {
		t.Fatal(err)
	}
	if !fresh.Status.EtcdMaintenance.InProgress {
		t.Fatal("uncertain operation released its reservation")
	}
	waiting, err := r.resumeMaintenance(t.Context(), fresh)
	if err != nil || !waiting {
		t.Fatalf("resume immediately: waiting=%v err=%v", waiting, err)
	}
	fresh.Status.EtcdMaintenance.LastAttemptTime = metav1.NewTime(time.Now().Add(-3 * time.Minute))
	if err := r.Status().Update(t.Context(), fresh); err != nil {
		t.Fatal(err)
	}
	f.healthErr = errors.New("previous member still unavailable")
	if waiting, err = r.resumeMaintenance(t.Context(), fresh); !waiting || err == nil {
		t.Fatalf("unhealthy resume: waiting=%v err=%v", waiting, err)
	}
	f.healthErr = nil
	if waiting, err = r.resumeMaintenance(t.Context(), fresh); waiting || err != nil {
		t.Fatalf("healthy resume: waiting=%v err=%v", waiting, err)
	}
	if err := r.reconcileMaintenance(t.Context(), fresh); err != nil {
		t.Fatal(err)
	}
	if len(f.defragged) != 1 {
		t.Fatal("interrupted operation started another defrag")
	}
}

func TestEtcdMaintenancePostHealthFailureKeepsReservation(t *testing.T) {
	r, ts, f := maintenanceFixture(t)
	f.afterDefrag = func() { f.healthErr = errors.New("member unhealthy after defrag") }
	if err := r.reconcileMaintenance(t.Context(), ts); err == nil {
		t.Fatal("expected post-defrag health failure")
	}
	fresh := &multigresv1alpha1.TopoServer{}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(ts), fresh); err != nil {
		t.Fatal(err)
	}
	if !fresh.Status.EtcdMaintenance.InProgress {
		t.Fatal("failed post-check released reservation")
	}
}
