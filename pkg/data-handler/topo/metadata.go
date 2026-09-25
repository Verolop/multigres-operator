package topo

import (
	"slices"

	"github.com/multigres/multigres/go/common/topoclient"
	clustermetadatapb "github.com/multigres/multigres/go/pb/clustermetadata"
	"google.golang.org/protobuf/proto"
)

// updateDatabaseMetadata compares only fields managed by the operator, inside
// the store's optimistic concurrency retry. Comparing before UpdateDatabaseFields
// would allow an intervening write to be missed. Preserve all other fields.
func updateDatabaseMetadata(existing, desired *clustermetadatapb.Database) error {
	existingCells := slices.Clone(existing.Cells)
	desiredCells := slices.Clone(desired.Cells)
	slices.Sort(existingCells)
	slices.Sort(desiredCells)
	if slices.Equal(existingCells, desiredCells) &&
		proto.Equal(existing.BackupLocation, desired.BackupLocation) &&
		proto.Equal(existing.BootstrapDurabilityPolicy, desired.BootstrapDurabilityPolicy) {
		return &topoclient.TopoError{Code: topoclient.NoUpdateNeeded}
	}
	existing.Cells = desiredCells
	existing.BackupLocation = desired.BackupLocation
	existing.BootstrapDurabilityPolicy = desired.BootstrapDurabilityPolicy
	return nil
}
