package com.mcbench.capture.io;

import com.mcbench.capture.model.RawEvent;

import java.io.BufferedOutputStream;
import java.io.ByteArrayOutputStream;
import java.io.IOException;
import java.io.OutputStream;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.StandardOpenOption;
import java.util.List;
import java.util.zip.Deflater;
import java.util.zip.DeflaterOutputStream;

/**
 * CaptureLogWriter appends event frames to a capture log file, matching the
 * format Go's {@code rawlog} package reads:
 *
 * <pre>
 *   [FrameLen u32 BE][FrameHeader][zlib CompressedPayload] ...
 *   FrameHeader: schema_version VarInt, server_id String, start_ms i64 BE, end_ms i64 BE
 *   payload (decompressed): repeated [event_len VarInt][RawEvent bytes]
 * </pre>
 *
 * Compression is zlib (RFC 1950) via java.util.zip.Deflater, which Go's
 * compress/zlib reads directly. Not thread-safe; the caller (WriterTask) must
 * serialize writes to one instance.
 */
public final class CaptureLogWriter implements AutoCloseable {
    private final int schemaVersion;
    private final byte[] serverIdUtf8;
    private OutputStream out;
    private Path currentPath;
    private long bytesWritten;

    // Reused across frames. One writer thread owns one CaptureLogWriter, so none
    // of this needs synchronising — and none of it is reallocated per frame.
    private final Deflater deflater = new Deflater(Deflater.DEFAULT_COMPRESSION, false);
    private byte[] zbuf = new byte[256 * 1024];
    private final ByteWriter header = new ByteWriter(64);
    private final byte[] lenPrefix = new byte[4];

    public CaptureLogWriter(int schemaVersion, String serverId) {
        this.schemaVersion = schemaVersion;
        this.serverIdUtf8 = (serverId == null ? "" : serverId)
                .getBytes(java.nio.charset.StandardCharsets.UTF_8);
    }

    /** Opens (or rotates to) a new capture log file. Closes any previous file. */
    public void open(Path path) throws IOException {
        close();
        Files.createDirectories(path.getParent());
        this.out = new BufferedOutputStream(Files.newOutputStream(path,
                StandardOpenOption.CREATE, StandardOpenOption.WRITE, StandardOpenOption.TRUNCATE_EXISTING),
                64 * 1024);
        this.currentPath = path;
        this.bytesWritten = 0;
    }

    public Path currentPath() { return currentPath; }
    public long bytesWritten() { return bytesWritten; }
    public boolean isOpen() { return out != null; }

    /**
     * Writes one frame containing the given events. startMs/endMs are the epoch
     * millis of the first and last event in the batch. Does nothing if empty.
     *
     * Convenience path for tests and fixtures; production uses
     * {@link #writeFrame(ByteWriter, int, long, long)}, which never materialises
     * a RawEvent.
     */
    public void writeFrame(List<RawEvent> events, long startMs, long endMs) throws IOException {
        if (events.isEmpty() || out == null) {
            return;
        }
        ByteWriter payload = new ByteWriter();
        ByteWriter ew = new ByteWriter();
        for (RawEvent e : events) {
            ew.reset();
            e.encode(ew);
            payload.varInt(ew.length());
            payload.raw(ew.array(), 0, ew.length());
        }
        writeFrame(payload, events.size(), startMs, endMs);
    }

    /**
     * Writes one frame from a pre-encoded payload buffer.
     *
     * Nothing here allocates per event, and nothing allocates per frame once the
     * buffers have reached steady size: the deflater is reset rather than
     * recreated, and its output goes into a buffer that is reused across frames.
     * Writer-thread garbage is not free — a young GC is stop-the-world, so it
     * stalls the tick just as surely as main-thread work would.
     */
    public void writeFrame(ByteWriter payload, int eventCount, long startMs, long endMs)
            throws IOException {
        if (eventCount == 0 || payload.length() == 0 || out == null) {
            return;
        }
        int compressedLen = deflate(payload.array(), payload.length());

        header.reset();
        header.varInt(schemaVersion);
        header.stringBytes(serverIdUtf8);
        header.int64BE(startMs);
        header.int64BE(endMs);

        long frameLen = (long) header.length() + compressedLen;
        lenPrefix[0] = (byte) ((frameLen >>> 24) & 0xFF);
        lenPrefix[1] = (byte) ((frameLen >>> 16) & 0xFF);
        lenPrefix[2] = (byte) ((frameLen >>> 8) & 0xFF);
        lenPrefix[3] = (byte) (frameLen & 0xFF);

        out.write(lenPrefix);
        out.write(header.array(), 0, header.length());
        out.write(zbuf, 0, compressedLen);
        out.flush();
        bytesWritten += 4 + frameLen;
    }

    /** Deflates into the reusable zbuf, growing it only if a frame outgrows it. */
    private int deflate(byte[] data, int len) {
        deflater.reset();
        deflater.setInput(data, 0, len);
        deflater.finish();
        int total = 0;
        while (!deflater.finished()) {
            if (total == zbuf.length) {
                byte[] bigger = new byte[zbuf.length * 2];
                System.arraycopy(zbuf, 0, bigger, 0, total);
                zbuf = bigger;
            }
            total += deflater.deflate(zbuf, total, zbuf.length - total);
        }
        return total;
    }

    @Override
    public void close() throws IOException {
        if (out != null) {
            out.close();
            out = null;
        }
    }

    /** Releases the deflater's native memory. Call once, at shutdown. */
    public void dispose() {
        deflater.end();
    }
}
