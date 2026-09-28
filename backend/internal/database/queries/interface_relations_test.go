package queries

import (
	"slices"
	"testing"
)

func TestInterfaceRelationsReplaceAndList(t *testing.T) {
	db := testDB(t)
	seedPortStatsDevice(t, db, "1")
	seedPortStatsDevice(t, db, "2")

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(ReplaceInterfaceRelations(db, "1", []InterfaceRelation{
		{InterfaceName: "ether2", Kind: "bond", Parent: "bond1"},
		{InterfaceName: "ether1", Kind: "bond", Parent: "bond1"},
		{InterfaceName: "pppoe-out1", Kind: "pppoe", Parent: "ether9"},
		{InterfaceName: "", Kind: "bond", Parent: "bond9"},                       // skipped
		{DeviceID: "2", InterfaceName: "vlan10", Kind: "vlan", Parent: "bridge"}, // device arg wins
	}))
	must(ReplaceInterfaceRelations(db, "2", []InterfaceRelation{{InterfaceName: "vlan20", Kind: "vlan", Parent: "ether1"}}))

	got, err := ListInterfaceRelations(db)
	must(err)
	want := []InterfaceRelation{
		{DeviceID: "1", InterfaceName: "ether1", Kind: "bond", Parent: "bond1"},
		{DeviceID: "1", InterfaceName: "ether2", Kind: "bond", Parent: "bond1"},
		{DeviceID: "1", InterfaceName: "pppoe-out1", Kind: "pppoe", Parent: "ether9"},
		{DeviceID: "1", InterfaceName: "vlan10", Kind: "vlan", Parent: "bridge"},
		{DeviceID: "2", InterfaceName: "vlan20", Kind: "vlan", Parent: "ether1"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("relations =\n %+v\nwant\n %+v", got, want)
	}

	// A new dump replaces the device's rows wholesale; others are untouched.
	must(ReplaceInterfaceRelations(db, "1", nil))
	got, err = ListInterfaceRelations(db)
	must(err)
	if len(got) != 1 || got[0].DeviceID != "2" {
		t.Fatalf("after empty dump: %+v", got)
	}

	must(DeleteDevice(db, "2"))
	if n := countRows(t, db, "interface_relations"); n != 0 {
		t.Errorf("rows after device delete = %d, want 0 (cascade)", n)
	}
}
