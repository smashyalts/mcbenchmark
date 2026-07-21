package mcproto

import (
	"fmt"

	"mcbench/internal/mcwire"
)

// Air block state ids. Read from the server's block report rather than assumed:
// only plain air is 0, and the two others are nowhere near it.
const (
	AirStateID     int32 = 0
	VoidAirStateID int32 = 15292
	CaveAirStateID int32 = 15293
)

// IsAir reports whether a block state is one of the three air states.
//
// This is the whole question a dig confirmation turns on, so it is worth being
// exact: treating only id 0 as air would count a dig into a cave (cave_air, id
// 15293) as having broken something.
func IsAir(state int32) bool {
	return state == AirStateID || state == VoidAirStateID || state == CaveAirStateID
}

// ChunkBlocks is the result of reading a level_chunk_with_light packet: the
// column's position, and the block states at the positions the caller asked
// for.
type ChunkBlocks struct {
	X, Z   int32
	States map[[3]int32]int32
	// Sections is how many chunk sections the column carried, which is how the
	// caller works out where the world starts vertically — the client is told
	// the world height in registry data it does not read.
	Sections int
}

// sectionEntries is the number of blocks in a chunk section (16^3).
const sectionEntries = 4096

// maxIndirectBitsStates is the largest bits-per-entry that still carries a
// palette for block states; above it the ids are written directly. Biomes
// switch over at 3 (their containers hold 64 entries, not 4096).
const (
	maxIndirectBitsStates = 8
	maxIndirectBitsBiomes = 3
)

// ParseChunkBlocks reads a level_chunk_with_light body and extracts the block
// states at the positions want() accepts.
//
// It parses rather than skips because there is no way to seek: every chunk
// section is variable-length, so reaching section 12 means walking sections 0
// through 11 — including their biome containers — byte by byte. What it does
// avoid is *storing* anything it was not asked for. A benchmark runs hundreds of
// bots, each holding a hundred-odd chunks in view; keeping full chunk data would
// be gigabytes, while the positions a trace actually touches number in the
// hundreds.
//
// The parse is self-checking. The section buffer is length-prefixed, so after
// reading every section the cursor must land exactly on its end; if it does not,
// this format has changed and the states read are meaningless. Returning an
// error there is the difference between the caller knowing it cannot verify a
// dig and the caller silently believing wrong data — which is the failure this
// whole ledger exists to remove.
//
// Layout, from the server's own writers (ClientboundLevelChunkPacketData.write,
// LevelChunkSection.write, PalettedContainer.Data.write):
//
//	i32 chunkX, i32 chunkZ
//	heightmaps: VarInt count, then count × (VarInt type, VarInt len, len × i64)
//	VarInt bufferLen, then bufferLen bytes of sections:
//	  per section:
//	    i16 nonEmptyBlockCount
//	    i16 fluidCount            <- added in 26.x; miss it and every section
//	                                 after the first decodes as noise
//	    block states container
//	    biomes container
//	container:
//	  u8 bitsPerEntry
//	  palette: bpe 0 -> VarInt single id; 1..max -> VarInt count + ids;
//	           above max -> none, ids are written directly
//	  long array, with NO length prefix — the count follows from bitsPerEntry
func ParseChunkBlocks(body []byte, minSectionY int32, want func(x, y, z int32) bool) (ChunkBlocks, error) {
	var out ChunkBlocks
	r := mcwire.NewReader(body)
	var err error
	if out.X, err = r.Int32BE(); err != nil {
		return out, fmt.Errorf("chunk x: %w", err)
	}
	if out.Z, err = r.Int32BE(); err != nil {
		return out, fmt.Errorf("chunk z: %w", err)
	}
	if err := skipHeightmaps(r); err != nil {
		return out, err
	}
	bufLen, err := r.VarInt()
	if err != nil {
		return out, fmt.Errorf("section buffer length: %w", err)
	}
	if bufLen < 0 || int(bufLen) > r.Remaining() {
		return out, fmt.Errorf("implausible section buffer length %d", bufLen)
	}
	buf, err := r.Bytes(int(bufLen))
	if err != nil {
		return out, err
	}

	out.States = make(map[[3]int32]int32)
	if want == nil {
		want = func(int32, int32, int32) bool { return false }
	}
	sr := mcwire.NewReader(buf)
	baseX, baseZ := out.X*16, out.Z*16
	section := int32(0)
	for ; sr.Remaining() > 0; section++ {
		if _, err := sr.Bytes(4); err != nil { // block count + fluid count
			return out, fmt.Errorf("section %d counts: %w", section, err)
		}
		sectionMinY := (minSectionY + section) * 16
		if err := readStates(sr, sectionMinY, baseX, baseZ, want, out.States); err != nil {
			return out, fmt.Errorf("section %d states: %w", section, err)
		}
		if err := skipContainer(sr, 64, maxIndirectBitsBiomes); err != nil {
			return out, fmt.Errorf("section %d biomes: %w", section, err)
		}
	}
	out.Sections = int(section)
	return out, nil
}

// CountChunkSections walks a chunk column without decoding any block, to learn
// how tall the world is. One extra walk, once per session — the answer is the
// same for every column and is cached after the first.
func CountChunkSections(body []byte) (int, error) {
	c, err := ParseChunkBlocks(body, 0, nil)
	return c.Sections, err
}

func skipHeightmaps(r *mcwire.Reader) error {
	n, err := r.VarInt()
	if err != nil {
		return fmt.Errorf("heightmap count: %w", err)
	}
	if n < 0 || int(n) > r.Remaining() {
		return fmt.Errorf("implausible heightmap count %d", n)
	}
	for i := int32(0); i < n; i++ {
		if _, err := r.VarInt(); err != nil { // heightmap type
			return fmt.Errorf("heightmap %d type: %w", i, err)
		}
		size, err := r.VarInt()
		if err != nil {
			return fmt.Errorf("heightmap %d size: %w", i, err)
		}
		if size < 0 || int(size)*8 > r.Remaining() {
			return fmt.Errorf("implausible heightmap size %d", size)
		}
		if _, err := r.Bytes(int(size) * 8); err != nil {
			return err
		}
	}
	return nil
}

// readStates decodes one block-state container, recording only wanted positions.
func readStates(r *mcwire.Reader, sectionMinY, baseX, baseZ int32,
	want func(x, y, z int32) bool, into map[[3]int32]int32) error {
	bpe, err := r.Byte()
	if err != nil {
		return err
	}
	palette, err := readPalette(r, int(bpe), maxIndirectBitsStates)
	if err != nil {
		return err
	}
	// A single-value section is the common case by far — most of a world is
	// solid stone or empty air — and it needs no bit unpacking at all.
	if bpe == 0 {
		state := palette[0]
		for i := 0; i < sectionEntries; i++ {
			x, y, z := unIndex(i, sectionMinY, baseX, baseZ)
			if want(x, y, z) {
				into[[3]int32{x, y, z}] = state
			}
		}
		return nil
	}

	longs := longsFor(int(bpe), sectionEntries)
	raw, err := r.Bytes(longs * 8)
	if err != nil {
		return err
	}
	perLong := 64 / int(bpe)
	mask := uint64(1)<<bpe - 1
	for i := 0; i < sectionEntries; i++ {
		x, y, z := unIndex(i, sectionMinY, baseX, baseZ)
		if !want(x, y, z) {
			continue
		}
		li := i / perLong
		off := uint((i % perLong) * int(bpe))
		v := uint64(0)
		for b := 0; b < 8; b++ { // big-endian long
			v = v<<8 | uint64(raw[li*8+b])
		}
		idx := int32((v >> off) & mask)
		if palette != nil {
			if int(idx) >= len(palette) {
				return fmt.Errorf("palette index %d out of range %d", idx, len(palette))
			}
			idx = palette[idx]
		}
		into[[3]int32{x, y, z}] = idx
	}
	return nil
}

// skipContainer walks a paletted container without decoding it.
func skipContainer(r *mcwire.Reader, entries, maxIndirect int) error {
	bpe, err := r.Byte()
	if err != nil {
		return err
	}
	if _, err := readPalette(r, int(bpe), maxIndirect); err != nil {
		return err
	}
	if bpe == 0 {
		return nil
	}
	_, err = r.Bytes(longsFor(int(bpe), entries) * 8)
	return err
}

// readPalette returns the id table, or nil when ids are written directly.
func readPalette(r *mcwire.Reader, bpe, maxIndirect int) ([]int32, error) {
	switch {
	case bpe == 0:
		v, err := r.VarInt()
		if err != nil {
			return nil, err
		}
		return []int32{v}, nil
	case bpe <= maxIndirect:
		n, err := r.VarInt()
		if err != nil {
			return nil, err
		}
		if n < 0 || int(n) > r.Remaining() {
			return nil, fmt.Errorf("implausible palette size %d", n)
		}
		out := make([]int32, n)
		for i := range out {
			if out[i], err = r.VarInt(); err != nil {
				return nil, err
			}
		}
		return out, nil
	default:
		return nil, nil // direct: the entries are global ids already
	}
}

// longsFor is how many 64-bit words hold `entries` values of bpe bits each.
// Entries never straddle a word boundary, so the spare high bits are padding.
func longsFor(bpe, entries int) int {
	perLong := 64 / bpe
	return (entries + perLong - 1) / perLong
}

// unIndex turns a section entry index into world coordinates. The order is
// y, then z, then x — the same order the server writes them.
func unIndex(i int, sectionMinY, baseX, baseZ int32) (x, y, z int32) {
	return baseX + int32(i&15), sectionMinY + int32(i>>8), baseZ + int32((i>>4)&15)
}

// ParseForgetChunk decodes forget_level_chunk, which carries a packed chunk
// position: z in the high half, x in the low half.
func ParseForgetChunk(body []byte) (x, z int32, err error) {
	r := mcwire.NewReader(body)
	v, err := r.Int64BE()
	if err != nil {
		return 0, 0, err
	}
	return int32(v), int32(v >> 32), nil
}

// ParseSectionBlocksUpdate decodes section_blocks_update: a section position
// then a packed (state, local position) per changed block.
func ParseSectionBlocksUpdate(body []byte) (map[[3]int32]int32, error) {
	r := mcwire.NewReader(body)
	pos, err := r.Int64BE()
	if err != nil {
		return nil, err
	}
	// 22 bits x, 20 bits y, 22 bits z, each signed.
	sx := int32(signExtend(pos>>42, 22))
	sy := int32(signExtend(pos<<44>>44, 20))
	sz := int32(signExtend(pos<<22>>42, 22))
	n, err := r.VarInt()
	if err != nil {
		return nil, err
	}
	if n < 0 || int(n) > r.Remaining() {
		return nil, fmt.Errorf("implausible section update count %d", n)
	}
	out := make(map[[3]int32]int32, n)
	for i := int32(0); i < n; i++ {
		v, err := r.VarLong()
		if err != nil {
			return nil, err
		}
		state := int32(uint64(v) >> 12)
		local := uint64(v) & 0xFFF
		x := sx*16 + int32((local>>8)&15)
		z := sz*16 + int32((local>>4)&15)
		y := sy*16 + int32(local&15)
		out[[3]int32{x, y, z}] = state
	}
	return out, nil
}
