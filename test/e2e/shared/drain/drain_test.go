//go:build e2e

package drain_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	multigresv1alpha1 "github.com/multigres/multigres-operator/api/v1alpha1"
	"github.com/multigres/multigres-operator/pkg/util/metadata"
	"github.com/multigres/multigres-operator/test/e2e/framework"
)

var cluster *framework.Cluster

func TestMain(m *testing.M) {
	var err error
	cluster, err = framework.EnsureSharedCluster()
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e setup: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// TestExternalPoolerDeletion verifies recovery after replica and primary deletion.
func TestExternalPoolerDeletion(t *testing.T) {
	ns := cluster.CreateNamespace(t)
	c, err := cluster.CRClient()
	if err != nil {
		t.Fatalf("create CR client: %v", err)
	}
	ctx := context.Background()

	cr := framework.MustLoadCluster("test/e2e/fixtures/base.yaml", ns)
	cr.Name = "external-pod-delete"
	replicas := int32(3)
	pool := cr.Spec.Databases[0].TableGroups[0].Shards[0].Spec.Pools["default"]
	pool.ReplicasPerCell = &replicas
	cr.Spec.Databases[0].TableGroups[0].Shards[0].Spec.Pools["default"] = pool
	if err := c.Create(ctx, cr); err != nil {
		t.Fatalf("create MultigresCluster: %v", err)
	}

	t.Log("waiting for the initial three-pooler shard to become healthy")
	cluster.WaitForAllPodsReady(t, ns)
	shard := waitForHealthyShard(t, c, ns, cr.Name)
	primary, replica := waitForPrimaryAndReplica(t, c, ns, cr.Name, shard)

	t.Logf("deleting replica %q", replica.Name)
	deleteAndWaitForReplacement(t, c, replica)
	shard = waitForHealthyShard(t, c, ns, cr.Name)
	primaryAfterReplica, _ := waitForPrimaryAndReplica(t, c, ns, cr.Name, shard)
	if primaryAfterReplica.Name != primary.Name {
		t.Fatalf("replica deletion changed primary from %q to %q", primary.Name, primaryAfterReplica.Name)
	}

	t.Logf("deleting primary %q", primaryAfterReplica.Name)
	deleteAndWaitForReplacement(t, c, primaryAfterReplica)
	shard = waitForHealthyShard(t, c, ns, cr.Name)
	primaryAfterFailover, _ := waitForPrimaryAndReplica(t, c, ns, cr.Name, shard)
	if primaryAfterFailover.Name == primaryAfterReplica.Name {
		t.Fatalf("primary %q was not replaced after deletion", primaryAfterReplica.Name)
	}
	t.Logf("failover completed with primary %q", primaryAfterFailover.Name)
}

// TestGracefulScaleDown verifies that a pool can scale down while remaining healthy.
func TestGracefulScaleDown(t *testing.T) {
	ns := cluster.CreateNamespace(t)
	c, err := cluster.CRClient()
	if err != nil {
		t.Fatalf("create CR client: %v", err)
	}
	ctx := context.Background()

	cr := framework.MustLoadCluster("test/e2e/fixtures/base.yaml", ns)
	cr.Name = "graceful-scale-down"
	replicas := int32(3)
	pool := cr.Spec.Databases[0].TableGroups[0].Shards[0].Spec.Pools["default"]
	pool.ReplicasPerCell = &replicas
	cr.Spec.Databases[0].TableGroups[0].Shards[0].Spec.Pools["default"] = pool
	if err := c.Create(ctx, cr); err != nil {
		t.Fatalf("create MultigresCluster: %v", err)
	}

	t.Log("waiting for the initial three-pooler shard to become healthy")
	cluster.WaitForAllPodsReady(t, ns)
	shard := waitForHealthyShard(t, c, ns, cr.Name)
	primary, _ := waitForPrimaryAndReplica(t, c, ns, cr.Name, shard)

	t.Log("scaling the pool from three poolers to two")
	if err := c.Get(ctx, client.ObjectKeyFromObject(cr), cr); err != nil {
		t.Fatalf("get MultigresCluster: %v", err)
	}
	replicas = 2
	pool = cr.Spec.Databases[0].TableGroups[0].Shards[0].Spec.Pools["default"]
	pool.ReplicasPerCell = &replicas
	cr.Spec.Databases[0].TableGroups[0].Shards[0].Spec.Pools["default"] = pool
	if err := c.Update(ctx, cr); err != nil {
		t.Fatalf("scale down MultigresCluster: %v", err)
	}

	waitForPoolPodCount(t, c, ns, cr.Name, 2)
	shard = waitForHealthyShard(t, c, ns, cr.Name)
	primaryAfterScaleDown, _ := waitForPrimaryAndReplica(t, c, ns, cr.Name, shard)
	if primaryAfterScaleDown.Name != primary.Name {
		t.Fatalf("scale-down changed primary from %q to %q", primary.Name, primaryAfterScaleDown.Name)
	}
}

func waitForHealthyShard(
	t testing.TB,
	c client.Client,
	namespace, clusterName string,
) *multigresv1alpha1.Shard {
	t.Helper()
	var found *multigresv1alpha1.Shard
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	err := wait.PollUntilContextCancel(ctx, 3*time.Second, true, func(ctx context.Context) (bool, error) {
		shards := &multigresv1alpha1.ShardList{}
		if err := c.List(ctx, shards,
			client.InNamespace(namespace),
			client.MatchingLabels{metadata.LabelMultigresCluster: clusterName},
		); err != nil || len(shards.Items) != 1 {
			return false, nil
		}
		shard := &shards.Items[0]
		if shard.Status.Phase != multigresv1alpha1.PhaseHealthy || !shard.Status.OrchReady {
			return false, nil
		}
		found = shard
		return true, nil
	})
	if err != nil {
		t.Fatalf("timed out waiting for healthy shard: %v", err)
	}
	return found
}

func waitForPrimaryAndReplica(
	t testing.TB,
	c client.Client,
	namespace, clusterName string,
	shard *multigresv1alpha1.Shard,
) (*corev1.Pod, *corev1.Pod) {
	t.Helper()
	var primary, replica *corev1.Pod
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	err := wait.PollUntilContextCancel(ctx, 3*time.Second, true, func(ctx context.Context) (bool, error) {
		freshShard := &multigresv1alpha1.Shard{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(shard), freshShard); err != nil {
			return false, nil
		}
		pods := &corev1.PodList{}
		if err := c.List(ctx, pods,
			client.InNamespace(namespace),
			client.MatchingLabels{
				metadata.LabelMultigresCluster: clusterName,
				metadata.LabelMultigresPool:    "default",
			},
		); err != nil {
			return false, nil
		}
		primary, replica = nil, nil
		for i := range pods.Items {
			pod := &pods.Items[i]
			if !podReady(pod) {
				return false, nil
			}
			switch freshShard.Status.PodRoles[pod.Name] {
			case "PRIMARY":
				primary = pod.DeepCopy()
			case "REPLICA":
				if replica == nil {
					replica = pod.DeepCopy()
				}
			}
		}
		return primary != nil && replica != nil, nil
	})
	if err != nil {
		t.Fatalf("timed out waiting for primary and replica: %v", err)
	}
	return primary, replica
}

func deleteAndWaitForReplacement(t testing.TB, c client.Client, pod *corev1.Pod) {
	t.Helper()
	ctx := context.Background()
	if err := c.Delete(ctx, pod); err != nil {
		t.Fatalf("delete pod %q: %v", pod.Name, err)
	}

	key := types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	err := wait.PollUntilContextCancel(waitCtx, 3*time.Second, true, func(ctx context.Context) (bool, error) {
		replacement := &corev1.Pod{}
		if err := c.Get(ctx, key, replacement); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		return replacement.UID != pod.UID && replacement.DeletionTimestamp.IsZero() && podReady(replacement), nil
	})
	if err != nil {
		t.Fatalf("timed out waiting for replacement of pod %q: %v", pod.Name, err)
	}
}

func waitForPoolPodCount(t testing.TB, c client.Client, namespace, clusterName string, want int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	err := wait.PollUntilContextCancel(ctx, 3*time.Second, true, func(ctx context.Context) (bool, error) {
		pods := &corev1.PodList{}
		if err := c.List(ctx, pods,
			client.InNamespace(namespace),
			client.MatchingLabels{
				metadata.LabelMultigresCluster: clusterName,
				metadata.LabelMultigresPool:    "default",
			},
		); err != nil {
			return false, err
		}
		if len(pods.Items) != want {
			return false, nil
		}
		for i := range pods.Items {
			if !pods.Items[i].DeletionTimestamp.IsZero() || !podReady(&pods.Items[i]) {
				return false, nil
			}
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("timed out waiting for %d ready pool pods: %v", want, err)
	}
}

func podReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}
