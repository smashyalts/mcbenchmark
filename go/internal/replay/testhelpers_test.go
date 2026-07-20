package replay

import "mcbench/internal/mcwire"

func varint(v int32) []byte                   { return mcwire.AppendVarInt(nil, v) }
func appendVarInt(dst []byte, v int32) []byte { return mcwire.AppendVarInt(dst, v) }

func appendString(dst []byte, s string) []byte {
	dst = mcwire.AppendVarInt(dst, int32(len(s)))
	return append(dst, s...)
}

func mcwireString(b []byte) (string, error) { return mcwire.NewReader(b).String() }

func appendF64(dst []byte, vs ...float64) []byte {
	w := mcwire.NewWriter()
	for _, v := range vs {
		w.Float64BE(v)
	}
	return append(dst, w.Bytes()...)
}

func appendF32(dst []byte, vs ...float32) []byte {
	w := mcwire.NewWriter()
	for _, v := range vs {
		w.Float32BE(v)
	}
	return append(dst, w.Bytes()...)
}
