package mcproto

import (
	"testing"

	"mcbench/internal/mcwire"
)

// buildChunk encodes a chunk column the way the server does, so the reader can
// be driven over all three palette shapes without a live server.
//
// It mirrors LevelChunkSection.write and PalettedContainer.Data.write field for
// field — including the two section counts and the length-prefix-free long
// array, the two details most easily got wrong.
func buildChunk(t *testing.T, cx, cz int32, sections []sectionSpec) []byte {
	t.Helper()
	w := mcwire.NewWriter()
	w.Int32BE(cx)
	w.Int32BE(cz)
	// Heightmaps: one entry, so the skip logic is actually exercised.
	w.VarInt(1)
	w.VarInt(1) // type
	w.VarInt(2) // long count
	w.Int64BE(0)
	w.Int64BE(0)

	body := mcwire.NewWriter()
	for _, sec := range sections {
		body.Uint16BE(4096) // non-empty block count
		body.Uint16BE(0)    // fluid count
		sec.encode(body)
		// Biomes: a single-value container, the cheapest legal encoding.
		body.Byte(0)
		body.VarInt(0)
	}
	w.VarInt(int32(body.Len()))
	w.Raw(body.Bytes())
	w.VarInt(0) // block entities
	return w.Bytes()
}

// sectionSpec describes one section's block states before encoding.
type sectionSpec struct {
	bpe     int     // 0 = single value
	palette []int32 // nil for direct
	states  []int32 // 4096 entries, palette indices or direct ids
}

func (s sectionSpec) encode(w *mcwire.Writer) {
	w.Byte(byte(s.bpe))
	if s.bpe == 0 {
		w.VarInt(s.palette[0])
		return
	}
	if s.palette != nil {
		w.VarInt(int32(len(s.palette)))
		for _, p := range s.palette {
			w.VarInt(p)
		}
	}
	perLong := 64 / s.bpe
	longs := (4096 + perLong - 1) / perLong
	acc := make([]uint64, longs)
	for i, v := range s.states {
		acc[i/perLong] |= uint64(uint32(v)) << uint((i%perLong)*s.bpe)
	}
	for _, l := range acc {
		w.Int64BE(int64(l))
	}
}

func single(id int32) sectionSpec {
	return sectionSpec{bpe: 0, palette: []int32{id}}
}

// A single-value section is most of a real world, and the cheapest thing to get
// wrong: it carries no long array at all.
func TestParseChunkSingleValueSections(t *testing.T) {
	body := buildChunk(t, 2, -3, []sectionSpec{single(1), single(0)})
	want := map[[3]int32]bool{{33, -64, -47}: true, {33, -48, -47}: true}
	got, err := ParseChunkBlocks(body, -4, func(x, y, z int32) bool {
		return want[[3]int32{x, y, z}]
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.X != 2 || got.Z != -3 || got.Sections != 2 {
		t.Fatalf("header wrong: %+v", got)
	}
	// Section 0 covers y -64..-49, section 1 covers -48..-33.
	if s := got.States[[3]int32{33, -64, -47}]; s != 1 {
		t.Errorf("section 0 block = %d, want 1", s)
	}
	if s := got.States[[3]int32{33, -48, -47}]; s != 0 {
		t.Errorf("section 1 block = %d, want 0 (air)", s)
	}
}

// An indirect palette is what a normal mixed section uses.
func TestParseChunkIndirectPalette(t *testing.T) {
	states := make([]int32, 4096)
	// Palette index 1 (stone) at exactly one position; the rest air.
	target := index(5, 9, 11)
	states[target] = 1
	sec := sectionSpec{bpe: 4, palette: []int32{0, 1234}, states: states}
	body := buildChunk(t, 0, 0, []sectionSpec{sec})

	got, err := ParseChunkBlocks(body, -4, func(x, y, z int32) bool { return true })
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if s := got.States[[3]int32{5, -64 + 9, 11}]; s != 1234 {
		t.Errorf("palette lookup = %d, want 1234", s)
	}
	if s := got.States[[3]int32{6, -64 + 9, 11}]; s != 0 {
		t.Errorf("neighbour = %d, want 0", s)
	}
}

// A direct-palette section writes global ids with no palette at all, and is
// what a section with many distinct blocks falls back to.
func TestParseChunkDirectPalette(t *testing.T) {
	states := make([]int32, 4096)
	target := index(1, 2, 3)
	states[target] = 15293 // cave_air, to also exercise IsAir
	sec := sectionSpec{bpe: 15, palette: nil, states: states}
	body := buildChunk(t, 0, 0, []sectionSpec{sec})

	got, err := ParseChunkBlocks(body, 0, func(x, y, z int32) bool { return true })
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	s := got.States[[3]int32{1, 2, 3}]
	if s != 15293 || !IsAir(s) {
		t.Errorf("direct id = %d (air=%v), want 15293 and air", s, IsAir(s))
	}
	if IsAir(1234) {
		t.Error("1234 must not be air")
	}
}

// The section buffer is length-prefixed, so a format change shows up as bytes
// left over after the last section — the reader keeps going and runs into
// nonsense. It must surface as an error, never as plausible garbage: the whole
// point of the ledger is that "cannot verify" differs from "verified".
func TestParseChunkRejectsTrailingBytesInSectionBuffer(t *testing.T) {
	w := mcwire.NewWriter()
	w.Int32BE(0)
	w.Int32BE(0)
	w.VarInt(0) // no heightmaps

	body := mcwire.NewWriter()
	body.Uint16BE(4096)
	body.Uint16BE(0)
	single(1).encode(body)
	body.Byte(0)
	body.VarInt(0)
	// Three bytes the format does not account for: too few to be a section.
	body.Raw([]byte{1, 2, 3})

	w.VarInt(int32(body.Len()))
	w.Raw(body.Bytes())
	w.VarInt(0)

	if _, err := ParseChunkBlocks(w.Bytes(), 0, func(int32, int32, int32) bool { return true }); err == nil {
		t.Fatal("trailing bytes in the section buffer must be an error, not ignored")
	}
}

// A truncated column must error rather than return what it managed to read.
func TestParseChunkRejectsTruncation(t *testing.T) {
	body := buildChunk(t, 0, 0, []sectionSpec{single(1), single(2)})
	for _, cut := range []int{4, 12, len(body) - 3} {
		if cut <= 0 || cut >= len(body) {
			continue
		}
		if _, err := ParseChunkBlocks(body[:cut], 0, func(int32, int32, int32) bool { return true }); err == nil {
			t.Errorf("truncating to %d bytes should fail", cut)
		}
	}
}

// index is the section entry order the server writes: y, then z, then x.
func index(x, y, z int) int { return y<<8 | z<<4 | x }
