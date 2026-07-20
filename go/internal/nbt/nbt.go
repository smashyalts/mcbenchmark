// Package nbt implements just enough of Minecraft's NBT format to read
// level.dat and write player data files.
//
// This is not a general-purpose NBT library: it exists so bench-playerdata can
// place benchmark accounts at a captured position before they ever connect.
// Doing that by hand is the only way to control where a replay bot spawns — the
// client cannot choose its own position, and a bot dropped at world spawn is
// both out of reach of every block its trace digs and, if spawn is not solid
// ground, kicked for "flying" after four seconds.
package nbt

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
)

// Tag IDs.
const (
	TagEnd byte = iota
	TagByte
	TagShort
	TagInt
	TagLong
	TagFloat
	TagDouble
	TagByteArray
	TagString
	TagList
	TagCompound
	TagIntArray
	TagLongArray
)

// Compound is a name -> value map. Values use the Go types below.
type Compound map[string]any

// List is a typed sequence; ElemType matters because NBT stores it explicitly
// and an empty list still has to declare one.
type List struct {
	ElemType byte
	Items    []any
}

// Read parses an NBT file, transparently gunzipping it. It returns the root
// compound; the root's own name is discarded (it is "" in every modern file).
func Read(path string) (Compound, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(raw) >= 2 && raw[0] == 0x1F && raw[1] == 0x8B {
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("%s: gunzip: %w", path, err)
		}
		raw, err = io.ReadAll(zr)
		if err != nil {
			return nil, fmt.Errorf("%s: gunzip: %w", path, err)
		}
	}
	r := &reader{buf: raw}
	id, err := r.u8()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if id != TagCompound {
		return nil, fmt.Errorf("%s: root tag is %d, want compound", path, id)
	}
	if _, err := r.str(); err != nil { // root name
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c, err := r.compound()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Write serialises root as a gzipped NBT file with an empty root name, writing
// via a temporary file so an interrupted run cannot leave a half-written .dat
// where the server expects a player.
func Write(path string, root Compound) error {
	var body bytes.Buffer
	w := &writer{buf: &body}
	w.u8(TagCompound)
	w.str("")
	if err := w.compound(root); err != nil {
		return err
	}
	var out bytes.Buffer
	zw := gzip.NewWriter(&out)
	if _, err := zw.Write(body.Bytes()); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out.Bytes(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Int looks up a nested int, e.g. Int(root, "Data", "DataVersion").
func Int(c Compound, path ...string) (int32, bool) {
	var cur any = c
	for _, k := range path {
		m, ok := cur.(Compound)
		if !ok {
			return 0, false
		}
		cur, ok = m[k]
		if !ok {
			return 0, false
		}
	}
	v, ok := cur.(int32)
	return v, ok
}

// ---- reading ----

type reader struct {
	buf []byte
	off int
}

func (r *reader) take(n int) ([]byte, error) {
	if n < 0 || len(r.buf)-r.off < n {
		return nil, fmt.Errorf("nbt: need %d bytes, have %d", n, len(r.buf)-r.off)
	}
	b := r.buf[r.off : r.off+n]
	r.off += n
	return b, nil
}

func (r *reader) u8() (byte, error) {
	b, err := r.take(1)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}

func (r *reader) u16() (uint16, error) {
	b, err := r.take(2)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint16(b), nil
}

func (r *reader) i32() (int32, error) {
	b, err := r.take(4)
	if err != nil {
		return 0, err
	}
	return int32(binary.BigEndian.Uint32(b)), nil
}

func (r *reader) str() (string, error) {
	n, err := r.u16()
	if err != nil {
		return "", err
	}
	b, err := r.take(int(n))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (r *reader) compound() (Compound, error) {
	c := Compound{}
	for {
		id, err := r.u8()
		if err != nil {
			return nil, err
		}
		if id == TagEnd {
			return c, nil
		}
		name, err := r.str()
		if err != nil {
			return nil, err
		}
		v, err := r.value(id)
		if err != nil {
			return nil, fmt.Errorf("tag %q: %w", name, err)
		}
		c[name] = v
	}
}

func (r *reader) value(id byte) (any, error) {
	switch id {
	case TagByte:
		v, err := r.u8()
		return int8(v), err
	case TagShort:
		v, err := r.u16()
		return int16(v), err
	case TagInt:
		return r.i32()
	case TagLong:
		b, err := r.take(8)
		if err != nil {
			return nil, err
		}
		return int64(binary.BigEndian.Uint64(b)), nil
	case TagFloat:
		b, err := r.take(4)
		if err != nil {
			return nil, err
		}
		return math.Float32frombits(binary.BigEndian.Uint32(b)), nil
	case TagDouble:
		b, err := r.take(8)
		if err != nil {
			return nil, err
		}
		return math.Float64frombits(binary.BigEndian.Uint64(b)), nil
	case TagByteArray:
		n, err := r.i32()
		if err != nil {
			return nil, err
		}
		b, err := r.take(int(n))
		if err != nil {
			return nil, err
		}
		return append([]byte(nil), b...), nil
	case TagString:
		return r.str()
	case TagList:
		et, err := r.u8()
		if err != nil {
			return nil, err
		}
		n, err := r.i32()
		if err != nil {
			return nil, err
		}
		if n < 0 {
			n = 0
		}
		l := List{ElemType: et, Items: make([]any, 0, min(int(n), 1024))}
		for i := int32(0); i < n; i++ {
			v, err := r.value(et)
			if err != nil {
				return nil, err
			}
			l.Items = append(l.Items, v)
		}
		return l, nil
	case TagCompound:
		return r.compound()
	case TagIntArray:
		n, err := r.i32()
		if err != nil {
			return nil, err
		}
		out := make([]int32, 0, min(int(n), 1024))
		for i := int32(0); i < n; i++ {
			v, err := r.i32()
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case TagLongArray:
		n, err := r.i32()
		if err != nil {
			return nil, err
		}
		out := make([]int64, 0, min(int(n), 1024))
		for i := int32(0); i < n; i++ {
			b, err := r.take(8)
			if err != nil {
				return nil, err
			}
			out = append(out, int64(binary.BigEndian.Uint64(b)))
		}
		return out, nil
	default:
		return nil, fmt.Errorf("nbt: unknown tag id %d", id)
	}
}

// ---- writing ----

type writer struct{ buf *bytes.Buffer }

func (w *writer) u8(v byte) { w.buf.WriteByte(v) }
func (w *writer) u16(v uint16) {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], v)
	w.buf.Write(b[:])
}
func (w *writer) i32(v int32) {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(v))
	w.buf.Write(b[:])
}
func (w *writer) i64(v int64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(v))
	w.buf.Write(b[:])
}
func (w *writer) str(s string) {
	w.u16(uint16(len(s)))
	w.buf.WriteString(s)
}

func (w *writer) compound(c Compound) error {
	for name, v := range c {
		id, err := tagID(v)
		if err != nil {
			return fmt.Errorf("tag %q: %w", name, err)
		}
		w.u8(id)
		w.str(name)
		if err := w.value(v); err != nil {
			return fmt.Errorf("tag %q: %w", name, err)
		}
	}
	w.u8(TagEnd)
	return nil
}

func tagID(v any) (byte, error) {
	switch v.(type) {
	case int8:
		return TagByte, nil
	case int16:
		return TagShort, nil
	case int32:
		return TagInt, nil
	case int64:
		return TagLong, nil
	case float32:
		return TagFloat, nil
	case float64:
		return TagDouble, nil
	case []byte:
		return TagByteArray, nil
	case string:
		return TagString, nil
	case List:
		return TagList, nil
	case Compound:
		return TagCompound, nil
	case []int32:
		return TagIntArray, nil
	case []int64:
		return TagLongArray, nil
	default:
		return 0, fmt.Errorf("nbt: cannot encode %T", v)
	}
}

func (w *writer) value(v any) error {
	switch t := v.(type) {
	case int8:
		w.u8(byte(t))
	case int16:
		w.u16(uint16(t))
	case int32:
		w.i32(t)
	case int64:
		w.i64(t)
	case float32:
		w.i32(int32(math.Float32bits(t)))
	case float64:
		w.i64(int64(math.Float64bits(t)))
	case []byte:
		w.i32(int32(len(t)))
		w.buf.Write(t)
	case string:
		w.str(t)
	case List:
		et := t.ElemType
		if len(t.Items) > 0 {
			id, err := tagID(t.Items[0])
			if err != nil {
				return err
			}
			et = id
		}
		if et == TagEnd && len(t.Items) > 0 {
			return fmt.Errorf("nbt: list has items but no element type")
		}
		w.u8(et)
		w.i32(int32(len(t.Items)))
		for _, it := range t.Items {
			if err := w.value(it); err != nil {
				return err
			}
		}
	case Compound:
		return w.compound(t)
	case []int32:
		w.i32(int32(len(t)))
		for _, x := range t {
			w.i32(x)
		}
	case []int64:
		w.i32(int32(len(t)))
		for _, x := range t {
			w.i64(x)
		}
	default:
		return fmt.Errorf("nbt: cannot encode %T", v)
	}
	return nil
}
