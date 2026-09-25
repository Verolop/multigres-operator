package v1alpha1

import (
	"fmt"
	"strconv"
	"time"
)

// EffectiveCompaction returns etcd's configured mode and retention, including
// defaults. Validate here as well as in the CRD for callers without admission.
func (c *EtcdMaintenanceConfig) EffectiveCompaction() (mode, retention string, err error) {
	mode = "periodic"
	if c != nil && c.AutoCompactionMode != "" {
		mode = c.AutoCompactionMode
	}
	switch mode {
	case "periodic":
		retention = "1h"
	case "revision":
		retention = "10000"
	default:
		return "", "", fmt.Errorf("invalid etcd compaction mode %q", mode)
	}
	if c != nil && c.AutoCompactionRetention != "" {
		retention = c.AutoCompactionRetention
	}
	if mode == "periodic" {
		d, parseErr := time.ParseDuration(retention)
		if parseErr != nil || d <= 0 {
			return "", "", fmt.Errorf("invalid periodic etcd compaction retention %q", retention)
		}
	} else {
		n, parseErr := strconv.ParseInt(retention, 10, 64)
		if parseErr != nil || n <= 0 {
			return "", "", fmt.Errorf("invalid revision etcd compaction retention %q", retention)
		}
	}
	return mode, retention, nil
}

// EffectiveQuotaBackendBytes preserves the pre-existing etcd quota unless an
// administrator explicitly supplies one. Quota cannot predict restore memory.
func (c *EtcdMaintenanceConfig) EffectiveQuotaBackendBytes() int64 {
	if c == nil || c.QuotaBackendBytes == nil {
		return 2 * 1024 * 1024 * 1024
	}
	return *c.QuotaBackendBytes
}

// DefragmentationIsEnabled reports whether automatic defragmentation is opted in.
func (c *EtcdMaintenanceConfig) DefragmentationIsEnabled() bool {
	return c != nil && c.DefragmentationEnabled != nil && *c.DefragmentationEnabled
}
