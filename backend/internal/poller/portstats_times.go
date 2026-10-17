package poller

import "time"

// DeviceSampleTimes reports when each device's interface counters were last
// read: the newest reading among its interfaces. The fleet snapshot carries
// one cycle time, while a device whose reads fail keeps its last rates for up
// to three intervals; these times tell such stale rates from fresh ones.
// A nil collector reports none.
func (c *PortStatsCollector) DeviceSampleTimes() map[string]time.Time {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]time.Time)
	for k, s := range c.samples {
		if s.at.After(out[k.device]) {
			out[k.device] = s.at
		}
	}
	return out
}
