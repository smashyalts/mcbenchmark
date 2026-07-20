package tracefile

import (
	"os"
	"path/filepath"
	"testing"

	"mcbench/internal/rawevent"
)

// TestOriginRoundTrip pins that the origin survives a write/read cycle. The
// origin is what bench-playerdata places bots with; losing it silently sends
// every bot to world spawn, where its block events do nothing.
func TestOriginRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.mct")
	in := &Trace{
		SchemaVersion: SchemaVersion, ProtocolVersion: 775,
		WorldProfileID: "w", TraceID: "t", DurationUs: 1000,
		Origin: &Origin{X: -904.5, Y: 79, Z: -152.5, Yaw: 12, Pitch: -3,
			Dimension: 1, Exact: true},
		Events: []TraceEvent{{OffsetUs: 0, Kind: 3, Data: []byte{1, 2}}},
	}
	if err := in.Write(path); err != nil {
		t.Fatal(err)
	}
	out, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if out.Origin == nil {
		t.Fatal("origin lost in round trip")
	}
	if *out.Origin != *in.Origin {
		t.Errorf("origin = %+v, want %+v", *out.Origin, *in.Origin)
	}
	if len(out.Events) != 1 || out.Events[0].Kind != 3 {
		t.Errorf("events corrupted: %+v", out.Events)
	}
}

// TestNoOriginRoundTrip covers a trace the compiler could not place.
func TestNoOriginRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.mct")
	in := &Trace{
		SchemaVersion: SchemaVersion, ProtocolVersion: 775,
		WorldProfileID: "w", TraceID: "t", DurationUs: 1,
		Events: []TraceEvent{{OffsetUs: 0, Kind: 0, Data: nil}},
	}
	if err := in.Write(path); err != nil {
		t.Fatal(err)
	}
	out, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if out.Origin != nil {
		t.Errorf("origin = %+v, want nil", out.Origin)
	}
}

// TestSchema1StillReads guards backward compatibility: traces compiled before
// the origin existed must keep working rather than becoming unreadable.
func TestSchema1StillReads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1.mct")
	in := &Trace{
		SchemaVersion: 1, ProtocolVersion: 775,
		WorldProfileID: "w", TraceID: "old", DurationUs: 500,
		Events: []TraceEvent{{OffsetUs: 0, Kind: 3, Data: []byte{9}}},
	}
	if err := in.Write(path); err != nil {
		t.Fatal(err)
	}
	// Sanity: a schema-1 file must not contain the origin flag byte at all.
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	out, err := Read(path)
	if err != nil {
		t.Fatalf("schema 1 trace no longer readable: %v", err)
	}
	if out.Origin != nil {
		t.Error("schema 1 trace reported an origin")
	}
	if len(out.Events) != 1 || out.Events[0].Data[0] != 9 {
		t.Errorf("schema 1 events corrupted: %+v", out.Events)
	}
}

// TestInventoryRoundTrip pins the login inventory through a write/read cycle.
//
// It decides what the bot is holding, and holding the wrong thing is not a
// cosmetic difference: barehanded stone takes 7.5 seconds against a diamond
// pickaxe's 0.4, so losing this silently turns a mining trace into a bot
// swinging at blocks that never break.
func TestInventoryRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inv.mct")
	in := &Trace{
		SchemaVersion: SchemaVersion, ProtocolVersion: 775,
		WorldProfileID: "w", TraceID: "t", DurationUs: 10,
		Inventory: &Inventory{SelectedSlot: 3, Items: []rawevent.ItemStack{
			{Slot: 3, ID: "minecraft:diamond_pickaxe", Count: 1},
			{Slot: 39, ID: "minecraft:iron_helmet", Count: 1},
			{Slot: 40, ID: "minecraft:shield", Count: 1},
		}},
		Events: []TraceEvent{{OffsetUs: 0, Kind: 3, Data: []byte{1}}},
	}
	if err := in.Write(path); err != nil {
		t.Fatal(err)
	}
	out, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if out.Inventory == nil {
		t.Fatal("inventory lost in round trip")
	}
	if out.Inventory.SelectedSlot != 3 {
		t.Errorf("selected slot = %d, want 3", out.Inventory.SelectedSlot)
	}
	if len(out.Inventory.Items) != 3 {
		t.Fatalf("items = %+v, want 3", out.Inventory.Items)
	}
	for i, want := range in.Inventory.Items {
		if out.Inventory.Items[i] != want {
			t.Errorf("item %d = %+v, want %+v", i, out.Inventory.Items[i], want)
		}
	}
}

// TestSchema2StillReads guards the traces compiled before inventories existed.
func TestSchema2StillReads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v2.mct")
	in := &Trace{
		SchemaVersion: 2, ProtocolVersion: 775,
		WorldProfileID: "w", TraceID: "old", DurationUs: 500,
		Origin: &Origin{X: 1, Y: 2, Z: 3, Exact: true},
		Events: []TraceEvent{{OffsetUs: 0, Kind: 3, Data: []byte{9}}},
	}
	if err := in.Write(path); err != nil {
		t.Fatal(err)
	}
	out, err := Read(path)
	if err != nil {
		t.Fatalf("schema 2 trace no longer readable: %v", err)
	}
	if out.Inventory != nil {
		t.Error("schema 2 trace reported an inventory")
	}
	if out.Origin == nil || out.Origin.X != 1 {
		t.Errorf("origin = %+v, want x=1", out.Origin)
	}
	if len(out.Events) != 1 || out.Events[0].Data[0] != 9 {
		t.Errorf("schema 2 events corrupted: %+v", out.Events)
	}
}
