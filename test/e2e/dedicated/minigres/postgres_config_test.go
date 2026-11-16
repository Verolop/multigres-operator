//go:build e2e

package minigres_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	multigresv1alpha1 "github.com/multigres/multigres-operator/api/v1alpha1"
	"github.com/multigres/multigres-operator/test/e2e/framework"
)

// TestMinigresPostgresConfig qualifies config rollout status against the running
// server: a reload keeps PostgreSQL alive, while a restart replaces the pod but
// preserves its data. Fake pod hashes and RPC responses cannot prove either.
func TestMinigresPostgresConfig(t *testing.T) {
	cr, pod := startLiteRuntime(t, "")
	ns := cr.Namespace
	c, err := cluster.CRClient()
	require.NoError(t, err)
	sqlEventually(t, ns, pod.Name, "SELECT 1", "1")

	// Check the requested config on the child Shard as well as its observed
	// generation, so an old settled status cannot satisfy a new cluster edit.
	waitForConfig := func(key, value string) *multigresv1alpha1.PostgresConfigStatus {
		t.Helper()
		var result *multigresv1alpha1.PostgresConfigStatus
		eventually(t, "Lite PostgreSQL config status", func() (bool, error) {
			shards := &multigresv1alpha1.ShardList{}
			if err := c.List(t.Context(), shards, client.InNamespace(ns),
				client.MatchingLabels{"multigres.com/cluster": cr.Name}); err != nil {
				return false, err
			}
			if len(shards.Items) != 1 {
				return false, fmt.Errorf("expected one Lite shard, found %d", len(shards.Items))
			}
			shard := &shards.Items[0]
			if key != "" && shard.Spec.PostgresConfig[key] != value {
				return false, nil
			}
			status := shard.Status.PostgresConfig
			condition := meta.FindStatusCondition(
				shard.Status.Conditions,
				multigresv1alpha1.ConditionUpToDate,
			)
			if shard.Status.ObservedGeneration != shard.Generation || status == nil ||
				status.InProgress || status.Error != "" || status.LastAppliedAt == nil ||
				condition == nil || condition.Status != "True" || condition.ObservedGeneration != shard.Generation {
				return false, fmt.Errorf(
					"config has not settled: status=%+v, UpToDate=%+v",
					status,
					condition,
				)
			}
			result = status.DeepCopy()
			return true, nil
		})
		return result
	}
	patchConfig := func(key, value string) {
		t.Helper()
		live := framework.GetCluster(t, c, ns, cr.Name)
		shard := live.Spec.Databases[0].TableGroups[0].Shards[0].Spec
		if shard.PostgresConfig == nil {
			shard.PostgresConfig = map[string]string{}
		}
		shard.PostgresConfig[key] = value
		framework.PatchCluster(t, c, live, framework.MustMarshal(map[string]any{
			"spec": map[string]any{"databases": live.Spec.Databases},
		}))
	}

	initial := waitForConfig("", "")
	pvc := dataPVC(t, ns, pod)
	sqlMust(
		t,
		ns,
		pod.Name,
		"CREATE TABLE config_e2e (id integer PRIMARY KEY, value text)",
		"CREATE TABLE",
	)
	sqlMust(
		t,
		ns,
		pod.Name,
		"INSERT INTO config_e2e VALUES (1, 'before config changes')",
		"INSERT 0 1",
	)
	started, err := sql(ns, pod.Name, "SELECT pg_postmaster_start_time()::text")
	require.NoError(t, err)
	previousWorkMem, err := sql(ns, pod.Name, "SHOW work_mem")
	require.NoError(t, err)
	require.NotEqual(t, "32MB", previousWorkMem, "reload must change the effective setting")

	patchConfig("work_mem", "32MB")
	sqlEventually(t, ns, pod.Name, "SHOW work_mem", "32MB")
	reloaded := waitForConfig("work_mem", "32MB")
	require.True(
		t,
		reloaded.LastAppliedAt.After(initial.LastAppliedAt.Time),
		"reload must advance lastAppliedAt",
	)
	require.Equal(t, pod.UID, getPod(t, ns, pod.Name).UID, "reload must keep the same pod")
	sqlMust(t, ns, pod.Name, "SELECT pg_postmaster_start_time()::text", started)
	sqlMust(t, ns, pod.Name, "SELECT value FROM config_e2e WHERE id = 1", "before config changes")
	t.Log(
		"reload: SQL work_mem=32MB, same pod and postmaster, config status settled with a newer lastAppliedAt",
	)

	previousMaxConnections, err := sql(ns, pod.Name, "SHOW max_connections")
	require.NoError(t, err)
	require.NotEqual(t, "200", previousMaxConnections, "restart must change the effective setting")
	patchConfig("max_connections", "200")
	replacement := waitPod(t, ns, cr.Name, pod.UID)
	sqlEventually(t, ns, replacement.Name, "SHOW max_connections", "200")
	restarted := waitForConfig("max_connections", "200")
	require.True(
		t,
		restarted.LastAppliedAt.After(reloaded.LastAppliedAt.Time),
		"restart must advance lastAppliedAt",
	)
	require.Equal(t, pvc.UID, dataPVC(t, ns, replacement).UID, "restart must reuse the data PVC")
	afterRestart, err := sql(ns, replacement.Name, "SELECT pg_postmaster_start_time()::text")
	require.NoError(t, err)
	require.NotEqual(t, started, afterRestart, "restart-only config must restart PostgreSQL")
	sqlMust(t, ns, replacement.Name, "SHOW work_mem", "32MB")
	sqlMust(
		t,
		ns,
		replacement.Name,
		"SELECT value FROM config_e2e WHERE id = 1",
		"before config changes",
	)
	sqlMust(
		t,
		ns,
		replacement.Name,
		"INSERT INTO config_e2e VALUES (2, 'after restart')",
		"INSERT 0 1",
	)
	sqlMust(t, ns, replacement.Name, "SELECT count(*) FROM config_e2e", "2")
	t.Log(
		"restart: SQL max_connections=200, new pod and postmaster, same PVC and persisted data, config status settled",
	)
}
