package resolver

import (
	"testing"

	multigresv1alpha1 "github.com/multigres/multigres-operator/api/v1alpha1"
	"k8s.io/utils/ptr"
)

func TestMergeEtcdMaintenance(t *testing.T) {
	base := &multigresv1alpha1.EtcdSpec{Maintenance: &multigresv1alpha1.EtcdMaintenanceConfig{
		AutoCompactionMode:      "periodic",
		AutoCompactionRetention: "6h",
		QuotaBackendBytes:       ptr.To(int64(1 << 30)),
		DefragmentationEnabled:  ptr.To(true),
	}}
	override := &multigresv1alpha1.EtcdSpec{
		Maintenance: &multigresv1alpha1.EtcdMaintenanceConfig{
			DefragmentationEnabled: ptr.To(false),
			QuotaBackendBytes:      ptr.To(int64(512 << 20)),
		},
	}
	mergeEtcdSpec(base, override)
	if base.Maintenance.AutoCompactionRetention != "6h" ||
		base.Maintenance.DefragmentationIsEnabled() ||
		base.Maintenance.EffectiveQuotaBackendBytes() != 512<<20 {
		t.Fatalf("incorrect merge: %+v", base.Maintenance)
	}
	*override.Maintenance.DefragmentationEnabled = true
	*override.Maintenance.QuotaBackendBytes = 2 << 30
	if base.Maintenance.DefragmentationIsEnabled() ||
		base.Maintenance.EffectiveQuotaBackendBytes() != 512<<20 {
		t.Fatal("merged maintenance aliases override")
	}
}
