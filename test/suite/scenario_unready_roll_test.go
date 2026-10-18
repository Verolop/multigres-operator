package suite

import (
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	multigresv1alpha1 "github.com/multigres/multigres-operator/api/v1alpha1"
	shardcontroller "github.com/multigres/multigres-operator/pkg/resource-handler/controller/shard"
	"github.com/multigres/multigres-operator/pkg/util/metadata"
	"github.com/multigres/testkit/ctrltest"
)

const fixedPostgresImage multigresv1alpha1.ImageRef = "example.com/postgres:fixed"

// TestRollForwardFromUnreadyPod pins that a rolling update never replaces a
// drifted pool pod that is unready, even when the new spec is exactly what
// would fix it: a pod crashlooping on a bad Postgres image holds back its own
// roll forward to a fixed one.
//
// Every rolling-update gate (isShardHealthy, maintenance capacity, the posture
// check) waits for Ready poolers, so a pod that is unready because of the
// very spec the rollout is replacing blocks its own replacement. On a cluster,
// recovery takes deleting the pod by hand.
//
// The crashlooping pod is made directly rather than by rolling the cluster
// onto a bad image, because this harness cannot roll a pod at all: the pooler
// sim registers every cohort under AtLeastN(1), so posture.CheckDisruption
// refuses every ordinary drain (see the pins in
// scenario_shard_lifecycle_test.go). The state is the same either way: one
// pool pod crashlooping on the image the shard is moving off.
//
// The pin awaits a design ruling as much as a fix, because the remedy is a
// policy choice rather than a mechanical one: replacing a drifted pod that is
// already unready costs no availability, but an unready pod is not always
// broken by its own spec. Any one of these retires it:
//   - the shard controller replaces a drifted pod that is not Ready without
//     waiting for shard health, as it already does for pods stranded on a
//     previous global topology;
//   - the rollout gains an explicit escape hatch, and this test is rewritten
//     to use it and assert the pod is replaced;
//   - a ruling that a manual delete is the intended recovery, and this test is
//     rewritten to perform it and assert the replacement comes up Ready.
func TestRollForwardFromUnreadyPod(t *testing.T) {
	c := newCase(t)
	cluster := c.MinimalCluster("unready-roll")
	c.WaitForClusterHealthy(cluster)

	pods, err := c.poolPods()
	c.NoError(err, "list pool pods")
	c.Len(pods, 2, "MinimalCluster's pool pods")
	broken := pods[1].Name
	badImage := containerImage(&pods[1], "postgres")

	c.FaultPods(func(pod *corev1.Pod) *ctrltest.PodFault {
		if pod.Name == broken && containerImage(pod, "postgres") == badImage {
			return &ctrltest.PodFault{CrashLoop: []string{"postgres"}}
		}
		return nil
	})
	c.Eventually(30*time.Second, "pod "+broken+" crashlooping", func() error {
		pod := &corev1.Pod{}
		if err := c.Get(client.ObjectKey{Namespace: c.NS, Name: broken}, pod); err != nil {
			return err
		}
		if podReady(pod) {
			return fmt.Errorf("pod %s is still Ready", broken)
		}
		return nil
	})

	c.updateCluster(cluster, func(got *MultigresCluster) {
		got.Spec.Images.Postgres = fixedPostgresImage
	})
	// Setup, not part of the pin: the pin asserts nothing unless the shard
	// provably wants the pod replaced.
	c.Eventually(30*time.Second, "the shard to report its pods drifted", func() error {
		shards := &ShardList{}
		if err := c.List(shards); err != nil {
			return err
		}
		for _, sh := range shards.Items {
			cond := meta.FindStatusCondition(sh.Status.Conditions, "RollingUpdate")
			if cond == nil || cond.Reason != "PodsDrifted" {
				return fmt.Errorf("shard %s RollingUpdate=%+v", sh.Name, cond)
			}
		}
		return nil
	})

	c.KnownDefect("MGO-ROLL-FORWARD-FROM-UNREADY", func() error {
		deadline := time.Now().Add(30 * time.Second)
		for {
			pod := &corev1.Pod{}
			err := c.Get(client.ObjectKey{Namespace: c.NS, Name: broken}, pod)
			if err != nil {
				c.Fatalf("get pod %s: %v", broken, err)
			}
			img := containerImage(pod, "postgres")
			if img == string(fixedPostgresImage) && podReady(pod) {
				return nil
			}
			if time.Now().After(deadline) {
				return fmt.Errorf(
					"pool pod %s is still on %s (Ready=%v) 30s after the fix was applied: "+
						"every rolling-update gate waits for Ready poolers, so a pod unready "+
						"because of its own spec holds back its own replacement",
					broken, img, podReady(pod))
			}
			time.Sleep(time.Second)
		}
	})
}

func (c *C) poolPods() ([]corev1.Pod, error) {
	pods := &corev1.PodList{}
	err := c.List(pods, client.MatchingLabels{
		metadata.LabelAppComponent: shardcontroller.PoolComponentName,
	})
	return pods.Items, err
}

// containerImage finds name among a pod's containers and init containers,
// since Postgres runs as a native sidecar.
func containerImage(pod *corev1.Pod, name string) string {
	for _, ctr := range append(pod.Spec.InitContainers, pod.Spec.Containers...) {
		if ctr.Name == name {
			return ctr.Image
		}
	}
	return ""
}
