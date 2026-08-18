package drain_test

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	multigresv1alpha1 "github.com/multigres/multigres-operator/api/v1alpha1"
	"github.com/multigres/multigres-operator/pkg/data-handler/drain"
	"github.com/multigres/multigres-operator/pkg/util/metadata"
)

func TestExecuteDrainStateMachine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		from string
		want string
	}{
		{name: "requested", from: metadata.DrainStateRequested, want: metadata.DrainStateDraining},
		{name: "draining", from: metadata.DrainStateDraining, want: metadata.DrainStateAcknowledged},
		{name: "acknowledged", from: metadata.DrainStateAcknowledged, want: metadata.DrainStateReadyForDeletion},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			shard, pod, k8sClient := testObjects(t, tt.from)

			requeue, err := drain.ExecuteDrainStateMachine(
				context.Background(), k8sClient, record.NewFakeRecorder(1), shard, pod,
			)
			if err != nil {
				t.Fatalf("execute drain state machine: %v", err)
			}
			if !requeue {
				t.Fatal("expected a requeue after a state transition")
			}

			updated := &corev1.Pod{}
			if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(pod), updated); err != nil {
				t.Fatalf("get updated pod: %v", err)
			}
			if got := updated.Annotations[metadata.AnnotationDrainState]; got != tt.want {
				t.Fatalf("drain state = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExecuteDrainStateMachineTimeout(t *testing.T) {
	shard, pod, k8sClient := testObjects(t, metadata.DrainStateDraining)
	pod.Annotations[metadata.AnnotationDrainRequestedAt] = time.Now().Add(-drain.DrainTimeout - time.Second).Format(time.RFC3339)
	if err := k8sClient.Update(context.Background(), pod); err != nil {
		t.Fatalf("update pod: %v", err)
	}

	requeue, err := drain.ExecuteDrainStateMachine(context.Background(), k8sClient, nil, shard, pod)
	if err != nil {
		t.Fatalf("execute timed out drain: %v", err)
	}
	if !requeue {
		t.Fatal("expected a requeue after the timeout transition")
	}

	updated := &corev1.Pod{}
	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(pod), updated); err != nil {
		t.Fatalf("get updated pod: %v", err)
	}
	if got := updated.Annotations[metadata.AnnotationDrainState]; got != metadata.DrainStateReadyForDeletion {
		t.Fatalf("drain state = %q, want %q", got, metadata.DrainStateReadyForDeletion)
	}
}

func TestExecuteDrainStateMachineNoop(t *testing.T) {
	for _, state := range []string{"", metadata.DrainStateReadyForDeletion} {
		t.Run(state, func(t *testing.T) {
			shard, pod, k8sClient := testObjects(t, state)
			requeue, err := drain.ExecuteDrainStateMachine(context.Background(), k8sClient, nil, shard, pod)
			if err != nil {
				t.Fatalf("execute drain state machine: %v", err)
			}
			if requeue {
				t.Fatal("did not expect a requeue")
			}
		})
	}
}

func testObjects(t testing.TB, state string) (*multigresv1alpha1.Shard, *corev1.Pod, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := multigresv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add Multigres scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}

	shard := &multigresv1alpha1.Shard{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "shard",
			Namespace: "default",
			Labels:    map[string]string{metadata.LabelMultigresCluster: "cluster"},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "pooler-0",
			Namespace:   shard.Namespace,
			Annotations: map[string]string{metadata.AnnotationDrainState: state},
		},
	}
	return shard, pod, fake.NewClientBuilder().WithScheme(scheme).WithObjects(shard, pod).Build()
}
