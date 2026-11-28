package routeros_test

import (
	"context"
	"slices"
	"testing"

	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/routeros/routerostest"
)

// PoolExec hands out the device's pooled session and nothing else: without
// a pool, or for a device the pool was never given, there is no link and
// nothing goes to any device; for a dialled device every read of the turn
// goes over its session.
func TestPoolExecHandsOutThePooledSession(t *testing.T) {
	ctx := context.Background()
	if run := (routeros.PoolExec{}).Device(ctx, "d1"); run != nil {
		t.Errorf("PoolExec without a pool handed out %#v", run)
	}

	pool := routeros.NewPool()
	t.Cleanup(pool.CloseAll)
	exec := routeros.PoolExec{Pool: pool}
	if run := exec.Device(ctx, "d1"); run != nil {
		t.Errorf("a device the pool was never given: handed out %#v", run)
	}

	fake := routerostest.NewDevice(t)
	fake.SetReply("/ip/arp/print", map[string]string{"address": "192.0.2.10", "mac-address": "02:00:00:00:00:0A"})
	fake.SetReply("/ip/dhcp-server/lease/print")
	fake.Dial(t, pool, "d1")
	run := exec.Device(ctx, "d1")
	if run == nil {
		t.Fatal("no link to a device the pool holds a session for")
	}
	arps, err := routeros.GetARPTable(run)
	if err != nil || len(arps) != 1 || arps[0].MAC != "02:00:00:00:00:0A" {
		t.Errorf("ARP read over the link = %+v, %v", arps, err)
	}
	if _, err := routeros.GetDHCPLeases(run); err != nil {
		t.Errorf("lease read over the link: %v", err)
	}
	if got, want := fake.Commands(), []string{"/ip/arp/print", "/ip/dhcp-server/lease/print"}; !slices.Equal(got, want) {
		t.Errorf("the device got %q, want %q", got, want)
	}
	if fake.Conns() != 1 {
		t.Errorf("the device saw %d connections, want the one pooled session", fake.Conns())
	}
}
