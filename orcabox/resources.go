package orcabox

import "github.com/QuantumNous/new-api/common"

// Use the same thresholds as New API's later relay guard, before reserving
// credits or reading an image body. Payment recovery stays available under load.
func resourcesReady() bool {
	c := common.GetPerformanceMonitorConfig()
	if !c.Enabled {
		return true
	}
	s := common.GetSystemStatus()
	return !(c.CPUThreshold > 0 && int(s.CPUUsage) > c.CPUThreshold || c.MemoryThreshold > 0 && int(s.MemoryUsage) > c.MemoryThreshold || c.DiskThreshold > 0 && int(s.DiskUsage) > c.DiskThreshold)
}
