// Package rawlog reads (and, for tests/fixtures, writes) the capture log
// file format produced by the Paper plugin:
//
//	[FrameLen u32 BE][FrameHeader][zlib CompressedPayload] ...
//
// FrameHeader: schema_version VarInt, server_id String, start_ms i64 BE,
// end_ms i64 BE. Decompressed payload: repeated [event_len VarInt][RawEvent].
//
// The payload uses zlib (RFC 1950) rather than LZ4 specifically because this
// is the Java<->Go boundary: Java writes it with java.util.zip.Deflater and Go
// reads it with compress/zlib, both standard libraries, so the two sides are
// guaranteed byte-compatible with no third-party frame-format concerns.
package rawlog

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"mcbench/internal/mcwire"
	"mcbench/internal/rawevent"
)

type FrameHeader struct {
	SchemaVersion int32
	ServerID      string
	StartMs       int64
	EndMs         int64
}

// ReadFile decodes every RawEvent in one capture log file.
func ReadFile(path string) ([]rawevent.RawEvent, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var events []rawevent.RawEvent
	off := 0
	frame := 0
	for off < len(data) {
		if len(data)-off < 4 {
			return events, fmt.Errorf("%s: truncated frame length at offset %d", path, off)
		}
		flen := int(binary.BigEndian.Uint32(data[off : off+4]))
		off += 4
		if flen < 0 || off+flen > len(data) {
			return events, fmt.Errorf("%s: frame %d length %d exceeds file", path, frame, flen)
		}
		fr := mcwire.NewReader(data[off : off+flen])
		off += flen
		var hdr FrameHeader
		if hdr.SchemaVersion, err = fr.VarInt(); err != nil {
			return events, fmt.Errorf("%s: frame %d header: %w", path, frame, err)
		}
		if hdr.ServerID, err = fr.String(); err != nil {
			return events, fmt.Errorf("%s: frame %d server_id: %w", path, frame, err)
		}
		if hdr.StartMs, err = fr.Int64BE(); err != nil {
			return events, fmt.Errorf("%s: frame %d start_ms: %w", path, frame, err)
		}
		if hdr.EndMs, err = fr.Int64BE(); err != nil {
			return events, fmt.Errorf("%s: frame %d end_ms: %w", path, frame, err)
		}
		zr, err := zlib.NewReader(bytes.NewReader(fr.Rest()))
		if err != nil {
			return events, fmt.Errorf("%s: frame %d zlib: %w", path, frame, err)
		}
		raw, err := io.ReadAll(zr)
		zr.Close()
		if err != nil {
			return events, fmt.Errorf("%s: frame %d decompress: %w", path, frame, err)
		}
		er := mcwire.NewReader(raw)
		for er.Remaining() > 0 {
			elen, err := er.VarInt()
			if err != nil {
				return events, fmt.Errorf("%s: frame %d event length: %w", path, frame, err)
			}
			eb, err := er.Bytes(int(elen))
			if err != nil {
				return events, fmt.Errorf("%s: frame %d event body: %w", path, frame, err)
			}
			ev, err := rawevent.Decode(mcwire.NewReader(eb))
			if err != nil {
				return events, fmt.Errorf("%s: frame %d event decode: %w", path, frame, err)
			}
			events = append(events, ev)
		}
		frame++
	}
	return events, nil
}

// ReadDir reads all raw-*.bin files in dir (sorted by name).
func ReadDir(dir string) ([]rawevent.RawEvent, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "raw-*.bin"))
	if err != nil {
		return nil, err
	}
	sort.Strings(matches)
	if len(matches) == 0 {
		return nil, fmt.Errorf("no raw-*.bin files found in %s", dir)
	}
	var all []rawevent.RawEvent
	for _, m := range matches {
		evs, err := ReadFile(m)
		if err != nil {
			return nil, err
		}
		all = append(all, evs...)
	}
	return all, nil
}

// WriteFile writes events as a single frame; used by tests and fixtures
// (the production writer is the Java plugin).
func WriteFile(path, serverID string, startMs, endMs int64, events []rawevent.RawEvent) error {
	payload := mcwire.NewWriter()
	for i := range events {
		ew := mcwire.NewWriter()
		events[i].Encode(ew)
		payload.VarInt(int32(ew.Len()))
		payload.Raw(ew.Bytes())
	}
	var comp bytes.Buffer
	zw := zlib.NewWriter(&comp)
	if _, err := zw.Write(payload.Bytes()); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	hdr := mcwire.NewWriter()
	hdr.VarInt(1)
	hdr.String(serverID)
	hdr.Int64BE(startMs)
	hdr.Int64BE(endMs)

	var out bytes.Buffer
	frameLen := uint32(hdr.Len() + comp.Len())
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], frameLen)
	out.Write(lenBuf[:])
	out.Write(hdr.Bytes())
	out.Write(comp.Bytes())
	return os.WriteFile(path, out.Bytes(), 0o644)
}

// Stream decodes a capture log file, invoking fn for each event, without
// retaining them.
//
// ReadFile returns every event in one slice, which is fine for fixtures and
// hopeless for a real capture: measured at 519 bytes of resident memory per
// event, a 1500-player hour (~108M events) projects to ~55 GB. Streaming keeps
// peak memory at one frame.
//
// The event passed to fn is only valid for the duration of the call — its
// payload aliases the decompressed frame buffer, which is reused.
func Stream(path string, fn func(rawevent.RawEvent) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 1<<20)

	var lenBuf [4]byte
	var frameBuf []byte
	frame := 0
	for {
		if _, err := io.ReadFull(br, lenBuf[:]); err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("%s: frame %d length: %w", path, frame, err)
		}
		flen := int(binary.BigEndian.Uint32(lenBuf[:]))
		if flen <= 0 {
			return fmt.Errorf("%s: frame %d invalid length %d", path, frame, flen)
		}
		if cap(frameBuf) < flen {
			frameBuf = make([]byte, flen)
		}
		buf := frameBuf[:flen]
		if _, err := io.ReadFull(br, buf); err != nil {
			return fmt.Errorf("%s: frame %d truncated (%w)", path, frame, err)
		}
		fr := mcwire.NewReader(buf)
		if _, err := fr.VarInt(); err != nil { // schema version
			return fmt.Errorf("%s: frame %d header: %w", path, frame, err)
		}
		if _, err := fr.String(); err != nil { // server id
			return fmt.Errorf("%s: frame %d server_id: %w", path, frame, err)
		}
		if _, err := fr.Int64BE(); err != nil { // start ms
			return fmt.Errorf("%s: frame %d start_ms: %w", path, frame, err)
		}
		if _, err := fr.Int64BE(); err != nil { // end ms
			return fmt.Errorf("%s: frame %d end_ms: %w", path, frame, err)
		}
		zr, err := zlib.NewReader(bytes.NewReader(fr.Rest()))
		if err != nil {
			return fmt.Errorf("%s: frame %d zlib: %w", path, frame, err)
		}
		raw, err := io.ReadAll(zr)
		zr.Close()
		if err != nil {
			return fmt.Errorf("%s: frame %d decompress: %w", path, frame, err)
		}
		er := mcwire.NewReader(raw)
		for er.Remaining() > 0 {
			elen, err := er.VarInt()
			if err != nil {
				return fmt.Errorf("%s: frame %d event length: %w", path, frame, err)
			}
			eb, err := er.Bytes(int(elen))
			if err != nil {
				return fmt.Errorf("%s: frame %d event body: %w", path, frame, err)
			}
			ev, err := rawevent.Decode(mcwire.NewReader(eb))
			if err != nil {
				return fmt.Errorf("%s: frame %d event decode: %w", path, frame, err)
			}
			if err := fn(ev); err != nil {
				return err
			}
		}
		frame++
	}
}

// StreamDir streams every raw-*.bin in dir, in name order.
func StreamDir(dir string, fn func(rawevent.RawEvent) error) error {
	matches, err := filepath.Glob(filepath.Join(dir, "raw-*.bin"))
	if err != nil {
		return err
	}
	sort.Strings(matches)
	if len(matches) == 0 {
		return fmt.Errorf("no raw-*.bin files found in %s", dir)
	}
	for _, m := range matches {
		if err := Stream(m, fn); err != nil {
			return err
		}
	}
	return nil
}
