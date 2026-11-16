package wav

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-audio/audio"
)

// countSeekWriter is an io.WriteSeeker that never stores the payload: writes
// only advance a position counter and seeks just move the cursor. It lets a
// test drive an encoder through the 4 GiB boundary without allocating the
// underlying audio bytes. The 28-byte header writes are kept in a map so the
// test can still inspect every backfilled value.
type countSeekWriter struct {
	pos     int64
	size    int64
	backing map[int64][]byte
}

func newCountSeekWriter() *countSeekWriter {
	return &countSeekWriter{backing: make(map[int64][]byte)}
}

func (c *countSeekWriter) Write(p []byte) (int, error) {
	if len(p) <= 64 {
		b := make([]byte, len(p))
		copy(b, p)
		c.backing[c.pos] = b
	}
	c.pos += int64(len(p))
	if c.pos > c.size {
		c.size = c.pos
	}
	return len(p), nil
}

func (c *countSeekWriter) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		c.pos = offset
	case io.SeekCurrent:
		c.pos += offset
	case io.SeekEnd:
		c.pos = c.size + offset
	}
	if c.pos < 0 {
		return 0, errors.New("negative position")
	}
	return c.pos, nil
}

// Extend grows the logical file size by n bytes without storing the payload,
// simulating the writes skipped by the overflow tests.
func (c *countSeekWriter) Extend(n int64) {
	c.size += n
}

func (c *countSeekWriter) u64At(t *testing.T, pos int64) uint64 {
	t.Helper()
	b, ok := c.backing[pos]
	if !ok || len(b) < 8 {
		t.Fatalf("no 8-byte backfill recorded at offset %d", pos)
	}
	return binary.LittleEndian.Uint64(b)
}

func (c *countSeekWriter) u32At(t *testing.T, pos int64) uint32 {
	t.Helper()
	b, ok := c.backing[pos]
	if !ok || len(b) < 4 {
		t.Fatalf("no 4-byte backfill recorded at offset %d", pos)
	}
	return binary.LittleEndian.Uint32(b)
}

// failSeekWriter wraps a writer whose Seek always fails.
type failSeekWriter struct {
	io.Writer
}

func (failSeekWriter) Seek(int64, int) (int64, error) {
	return 0, errors.New("seek is not supported")
}

// pcmBuffer builds an IntBuffer of n frames with deterministic values.
func pcmBuffer(sampleRate, numChans, bitDepth, n int) *audio.IntBuffer {
	data := make([]int, n*numChans)
	for i := range data {
		data[i] = int(int16(i*7 + 1))
	}
	return &audio.IntBuffer{
		Format:         &audio.Format{NumChannels: numChans, SampleRate: sampleRate},
		Data:           data,
		SourceBitDepth: bitDepth,
	}
}

// TestRF64SmallFileRoundTrip makes sure an explicitly enabled RF64 output
// remains valid for small content and round-trips sample-by-sample.
func TestRF64SmallFileRoundTrip(t *testing.T) {
	buf := new(bytes.Buffer)
	ws := newBufferWriteSeeker(buf)
	e := NewEncoder(ws, 44100, 16, 2, 1)
	e.RF64 = true
	in := pcmBuffer(44100, 2, 16, 1000)
	if err := e.Write(in); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	raw := buf.Bytes()

	// Binary header assertions.
	if string(raw[0:4]) != "RF64" {
		t.Fatalf("expected RF64 FourCC, got %q", raw[0:4])
	}
	if got := binary.LittleEndian.Uint32(raw[4:8]); got != rf64SizeSentinel {
		t.Fatalf("RF64 root size must be the sentinel, got 0x%08X", got)
	}
	if string(raw[8:12]) != "WAVE" {
		t.Fatalf("expected WAVE FourCC, got %q", raw[8:12])
	}
	if string(raw[12:16]) != "ds64" {
		t.Fatalf("ds64 must be the first chunk, got %q", raw[12:16])
	}
	if got := binary.LittleEndian.Uint32(raw[16:20]); got != uint32(ds64FixedSize) {
		t.Fatalf("unexpected ds64 chunk size %d", got)
	}
	riffSize64 := binary.LittleEndian.Uint64(raw[20:28])
	dataSize64 := binary.LittleEndian.Uint64(raw[28:36])
	sampleCount64 := binary.LittleEndian.Uint64(raw[36:44])
	tableLen := binary.LittleEndian.Uint32(raw[44:48])
	if tableLen != 0 {
		t.Fatalf("expected no ds64 table entries, got %d", tableLen)
	}
	const dataBytes = 1000 * 2 * 2 // frames * chans * bytesPerSample
	if dataSize64 != dataBytes {
		t.Fatalf("ds64 data size: expected %d, got %d", dataBytes, dataSize64)
	}
	if sampleCount64 != 1000 {
		t.Fatalf("ds64 sample count: expected 1000, got %d", sampleCount64)
	}
	if riffSize64 != uint64(len(raw)-8) {
		t.Fatalf("ds64 RIFF size: expected %d, got %d", len(raw)-8, riffSize64)
	}
	// Small files carry the real size in the 32-bit data chunk header too.
	dataChunkPos := int64(12 + 8 + ds64FixedSize + 8 + 16)
	if string(raw[dataChunkPos:dataChunkPos+4]) != "data" {
		t.Fatalf("expected data chunk at offset %d, got %q", dataChunkPos, raw[dataChunkPos:dataChunkPos+4])
	}
	if got := binary.LittleEndian.Uint32(raw[dataChunkPos+4 : dataChunkPos+8]); got != dataBytes {
		t.Fatalf("32-bit data size: expected %d, got %d", dataBytes, got)
	}

	d := NewDecoder(bytes.NewReader(raw))
	if err := d.FwdToPCM(); err != nil {
		t.Fatal(err)
	}
	if !d.IsRF64() {
		t.Fatal("decoder did not identify the file as RF64")
	}
	if d.PCMLen() != dataBytes {
		t.Fatalf("PCMLen: expected %d, got %d", dataBytes, d.PCMLen())
	}
	out, err := d.FullPCMBuffer()
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Data) != len(in.Data) {
		t.Fatalf("sample count: expected %d, got %d", len(in.Data), len(out.Data))
	}
	for i := range in.Data {
		if in.Data[i] != out.Data[i] {
			t.Fatalf("sample %d mismatch: %d != %d", i, in.Data[i], out.Data[i])
		}
	}
	if dur, err := d.Duration(); err != nil || dur == 0 {
		t.Fatalf("expected a non-zero duration, got %v (err: %v)", dur, err)
	}

	// A fresh decoder must accept the file as valid without fully reading it.
	dValid := NewDecoder(bytes.NewReader(raw))
	if !dValid.IsValidFile() {
		t.Fatal("RF64 file was rejected by IsValidFile")
	}
	if !dValid.IsRF64() {
		t.Fatal("IsValidFile did not leave the decoder in RF64 mode")
	}

	// Rewind must re-parse the RF64 headers and the ds64 chunk from scratch.
	if err := d.Rewind(); err != nil {
		t.Fatal(err)
	}
	if !d.IsRF64() || d.ds64 == nil {
		t.Fatal("RF64 state was lost after Rewind")
	}
	rewound := &audio.IntBuffer{
		Format: &audio.Format{NumChannels: 2, SampleRate: 44100},
		Data:   make([]int, len(in.Data)),
	}
	n, err := d.PCMBuffer(rewound)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(in.Data) {
		t.Fatalf("rewind read %d samples, expected %d", n, len(in.Data))
	}
	for i := range in.Data {
		if in.Data[i] != rewound.Data[i] {
			t.Fatalf("rewound sample %d mismatch: %d != %d", i, in.Data[i], rewound.Data[i])
		}
	}
}

// TestRF644GiBOverflowWithCounter uses a counting writer to cross the 4 GiB
// boundary without allocating any audio payload, then checks every backfilled
// field and the sentinel usage.
func TestRF644GiBOverflowWithCounter(t *testing.T) {
	const (
		numChans   = 2
		bitDepth   = 16
		frameBytes = numChans * bitDepth / 8 // 4 bytes/frame
	)
	// Just over the 32-bit limit: the smallest frame count whose payload
	// does not fit in a uint32. One frame is written through WriteFrame
	// first, the manual counter below sets the total.
	frames := int(math.MaxUint32)/frameBytes + 1
	targetDataBytes := uint64(frames) * frameBytes

	cw := newCountSeekWriter()
	e := NewEncoder(cw, 48000, bitDepth, numChans, 1)
	e.RF64 = true
	// One frame at a time keeps the encoder's internal byte buffer tiny.
	if err := e.WriteFrame(int16(1)); err != nil {
		t.Fatal(err)
	}
	// Force the frame counter to the overflowing value without a multi-GiB
	// payload; the counting writer still observes the full logical size.
	e.frames = frames
	// Account for the PCM bytes that would have streamed through the writer:
	// everything except the one frame really written, plus header overhead
	// already counted by the encoder.
	skipped := int64(targetDataBytes) - frameBytes
	cw.Extend(skipped)
	e.WrittenBytes += int(skipped)
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	if cw.u32At(t, 4) != rf64SizeSentinel {
		t.Fatal("RF64 root 32-bit size must stay at the sentinel")
	}
	if got := cw.u64At(t, e.ds64RiffSizePos); got != uint64(cw.size)-8 {
		t.Fatalf("ds64 RIFF size: expected %d, got %d", cw.size-8, got)
	}
	if got := cw.u64At(t, e.ds64DataSizePos); got != targetDataBytes {
		t.Fatalf("ds64 data size: expected %d, got %d", targetDataBytes, got)
	}
	if got := cw.u64At(t, e.ds64SampleCountPos); got != uint64(frames) {
		t.Fatalf("ds64 sample count: expected %d, got %d", frames, got)
	}
	if got := cw.u32At(t, int64(e.pcmChunkSizePos)); got != rf64SizeSentinel {
		t.Fatalf("overflowing data chunk size must be the sentinel, got 0x%08X", got)
	}
	if cw.size < int64(targetDataBytes) {
		t.Fatalf("logical file size %d smaller than the data payload %d", cw.size, targetDataBytes)
	}
}

// TestRF64SparseFile4GiB creates a real sparse file whose data chunk crosses
// the 4 GiB mark and decodes it back, checking chunked PCM reads and the
// even-byte alignment handling still work.
func TestRF64SparseFile4GiB(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "large.rf64.wav")

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	e := NewEncoder(f, 48000, 16, 2, 1)
	e.RF64 = true
	head := pcmBuffer(48000, 2, 16, 8)
	if err := e.Write(head); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	headerLen, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		t.Fatal(err)
	}
	// Extend the sparse file so the data payload ends past 4 GiB. The 8
	// head frames were written for real and are included in the payload.
	totalFrames := int(math.MaxUint32)/4 + 2
	targetDataBytes := uint64(totalFrames) * 4
	if err := f.Truncate(headerLen + int64(targetDataBytes)); err != nil {
		t.Fatal(err)
	}
	// Re-open an encoder on the same file: the sparse hole means no disk
	// allocation, and Close backfills the real 64-bit sizes.
	e2 := NewEncoder(f, 48000, 16, 2, 1)
	e2.RF64 = true
	e2.wroteHeader = true
	e2.pcmChunkStarted = true
	e2.pcmChunkSizePos = e.pcmChunkSizePos
	e2.ds64RiffSizePos = e.ds64RiffSizePos
	e2.ds64DataSizePos = e.ds64DataSizePos
	e2.ds64SampleCountPos = e.ds64SampleCountPos
	e2.WrittenBytes = int(headerLen + int64(targetDataBytes))
	e2.frames = totalFrames
	if err := e2.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()

	// Confirm the file really is sparse (or at least not fully allocated).
	var st os.FileInfo
	if st, err = os.Stat(path); err != nil {
		t.Fatal(err)
	}
	logicalSize := headerLen + int64(targetDataBytes)
	if st.Size() != logicalSize {
		t.Fatalf("file size: expected %d, got %d", logicalSize, st.Size())
	}
	// On POSIX verify the hole is not physically stored; a no-op elsewhere.
	assertSparseAllocation(t, path, logicalSize)

	rf, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer rf.Close()

	d := NewDecoder(rf)
	if err := d.FwdToPCM(); err != nil {
		t.Fatal(err)
	}
	if !d.IsRF64() {
		t.Fatal("expected RF64 detection on the sparse file")
	}
	if d.PCMLen() != int64(targetDataBytes) {
		t.Fatalf("PCMLen: expected %d, got %d", targetDataBytes, d.PCMLen())
	}
	if d.ds64.sampleCount64 != targetDataBytes/4 {
		t.Fatalf("sample count: expected %d, got %d", targetDataBytes/4, d.ds64.sampleCount64)
	}
	// Chunked read of the head samples, which were actually written.
	readBuf := &audio.IntBuffer{
		Format: &audio.Format{NumChannels: 2, SampleRate: 48000},
		Data:   make([]int, 16),
	}
	n, err := d.PCMBuffer(readBuf)
	if err != nil {
		t.Fatal(err)
	}
	if n != 16 {
		t.Fatalf("expected 16 samples, got %d", n)
	}
	for i := 0; i < 16; i++ {
		if readBuf.Data[i] != head.Data[i] {
			t.Fatalf("head sample %d mismatch: %d != %d", i, head.Data[i], readBuf.Data[i])
		}
	}
}

// TestClassicRIFFRegression checks that the default encoder still emits a
// classic RIFF header and no ds64 chunk.
func TestClassicRIFFRegression(t *testing.T) {
	buf := new(bytes.Buffer)
	ws := newBufferWriteSeeker(buf)
	e := NewEncoder(ws, 44100, 16, 1, 1)
	if err := e.Write(pcmBuffer(44100, 1, 16, 32)); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	raw := buf.Bytes()
	if string(raw[0:4]) != "RIFF" {
		t.Fatalf("expected RIFF FourCC, got %q", raw[0:4])
	}
	if bytes.Contains(raw[:40], []byte("ds64")) {
		t.Fatal("classic RIFF output must not contain a ds64 chunk")
	}
	if got := binary.LittleEndian.Uint32(raw[4:8]); got != uint32(len(raw)-8) {
		t.Fatalf("RIFF size: expected %d, got %d", len(raw)-8, got)
	}
	d := NewDecoder(bytes.NewReader(raw))
	if d.IsRF64() {
		t.Fatal("a classic RIFF file must not be reported as RF64")
	}
	if err := d.FwdToPCM(); err != nil {
		t.Fatal(err)
	}
	if d.PCMLen() != 32*2 {
		t.Fatalf("unexpected PCM length %d", d.PCMLen())
	}
}

// TestClassicRIFFOverflowRejected ensures an overflowing classic RIFF output
// fails explicitly instead of wrapping the 32-bit size fields.
func TestClassicRIFFOverflowRejected(t *testing.T) {
	cw := newCountSeekWriter()
	e := NewEncoder(cw, 48000, 16, 2, 1)
	if err := e.WriteFrame(int16(1)); err != nil {
		t.Fatal(err)
	}
	e.frames = (math.MaxUint32 / 4) + 1 // payload exceeds 4 GiB
	err := e.Close()
	if !errors.Is(err, ErrRIFFSizeOverflow) && !errors.Is(err, ErrPCMSizeOverflow) {
		t.Fatalf("expected an overflow error, got %v", err)
	}
}

// TestRF64NonSeekWriter returns an explicit error when the output cannot be
// seeked, so no half-written RF64 file is silently produced.
func TestRF64NonSeekWriter(t *testing.T) {
	var buf bytes.Buffer
	e := NewEncoder(failSeekWriter{Writer: &buf}, 44100, 16, 1, 1)
	e.RF64 = true
	err := e.Write(pcmBuffer(44100, 1, 16, 4))
	if !errors.Is(err, ErrRF64RequiresSeek) {
		t.Fatalf("expected ErrRF64RequiresSeek on header write, got %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("no byte should have been written before the seek failure, got %d", buf.Len())
	}
}

// TestRF64DecoderErrors covers malformed RF64 inputs: missing/truncated ds64,
// contradictory lengths and bad sentinels.
func TestRF64DecoderErrors(t *testing.T) {
	build := func(t *testing.T, mutate func(raw []byte)) []byte {
		t.Helper()
		buf := new(bytes.Buffer)
		ws := newBufferWriteSeeker(buf)
		e := NewEncoder(ws, 44100, 16, 1, 1)
		e.RF64 = true
		if err := e.Write(pcmBuffer(44100, 1, 16, 16)); err != nil {
			t.Fatal(err)
		}
		if err := e.Close(); err != nil {
			t.Fatal(err)
		}
		raw := append([]byte(nil), buf.Bytes()...)
		mutate(raw)
		return raw
	}

	t.Run("missing ds64", func(t *testing.T) {
		raw := build(t, func(raw []byte) {
			copy(raw[12:16], []byte("junk"))
		})
		d := NewDecoder(bytes.NewReader(raw))
		err := d.FwdToPCM()
		if !errors.Is(err, ErrMissingDS64) {
			t.Fatalf("expected ErrMissingDS64, got %v", err)
		}
	})

	t.Run("truncated ds64 body", func(t *testing.T) {
		raw := build(t, func(raw []byte) {
			// Claim a ds64 body larger than the file actually provides.
			binary.LittleEndian.PutUint32(raw[16:20], math.MaxUint32)
		})
		d := NewDecoder(bytes.NewReader(raw))
		err := d.FwdToPCM()
		if !errors.Is(err, ErrTruncatedDS64) {
			t.Fatalf("expected ErrTruncatedDS64, got %v", err)
		}
	})

	t.Run("truncated ds64 header", func(t *testing.T) {
		raw := build(t, func(raw []byte) {
			// ds64 chunk size below the 28-byte minimum.
			binary.LittleEndian.PutUint32(raw[16:20], 12)
		})
		d := NewDecoder(bytes.NewReader(raw))
		err := d.FwdToPCM()
		if !errors.Is(err, ErrTruncatedDS64) {
			t.Fatalf("expected ErrTruncatedDS64, got %v", err)
		}
	})

	t.Run("contradictory data size", func(t *testing.T) {
		raw := build(t, func(raw []byte) {
			binary.LittleEndian.PutUint64(raw[28:36], 999999)
		})
		d := NewDecoder(bytes.NewReader(raw))
		err := d.FwdToPCM()
		if !errors.Is(err, ErrDS64Contradiction) {
			t.Fatalf("expected ErrDS64Contradiction, got %v", err)
		}
	})

	t.Run("contradictory sample count", func(t *testing.T) {
		raw := build(t, func(raw []byte) {
			binary.LittleEndian.PutUint64(raw[36:44], 42)
		})
		d := NewDecoder(bytes.NewReader(raw))
		err := d.FwdToPCM()
		if !errors.Is(err, ErrDS64Contradiction) {
			t.Fatalf("expected ErrDS64Contradiction, got %v", err)
		}
	})

	t.Run("contradictory file size", func(t *testing.T) {
		raw := build(t, func(raw []byte) {
			// Inflate the ds64 RIFF size so it no longer matches the reader.
			binary.LittleEndian.PutUint64(raw[20:28], binary.LittleEndian.Uint64(raw[20:28])+100)
		})
		d := NewDecoder(bytes.NewReader(raw))
		err := d.FwdToPCM()
		if !errors.Is(err, ErrDS64Contradiction) {
			t.Fatalf("expected ErrDS64Contradiction, got %v", err)
		}
	})

	t.Run("bad root sentinel", func(t *testing.T) {
		raw := build(t, func(raw []byte) {
			binary.LittleEndian.PutUint32(raw[4:8], 1234)
		})
		d := NewDecoder(bytes.NewReader(raw))
		err := d.FwdToPCM()
		if !errors.Is(err, ErrRF64BadSentinel) {
			t.Fatalf("expected ErrRF64BadSentinel, got %v", err)
		}
	})
}

// TestRF64OddChunkAlignment builds an RF64 file whose data chunk has an odd
// size and verifies the decoder applies the mandatory word-alignment padding
// byte before locating the following chunk.
func TestRF64OddChunkAlignment(t *testing.T) {
	var buf bytes.Buffer
	ws := newBufferWriteSeeker(&buf)
	// 8-bit mono, 3 samples => an odd 3-byte data payload.
	e := NewEncoder(ws, 8000, 8, 1, 1)
	e.RF64 = true
	if err := e.Write(&audio.IntBuffer{
		Format: &audio.Format{NumChannels: 1, SampleRate: 8000},
		Data:   []int{10, 20, 30},
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	raw := buf.Bytes()

	d := NewDecoder(bytes.NewReader(raw))
	if err := d.FwdToPCM(); err != nil {
		t.Fatal(err)
	}
	if d.PCMLen() != 3 {
		t.Fatalf("expected an unpadded PCM length of 3, got %d", d.PCMLen())
	}
	out, err := d.FullPCMBuffer()
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Data) != 3 || out.Data[0] != 10 || out.Data[1] != 20 || out.Data[2] != 30 {
		t.Fatalf("unexpected samples: %v", out.Data)
	}

	// Append a trailing custom chunk behind the odd-sized data payload, with
	// the required single zero padding byte in between.
	trailing := []byte{'t', 'e', 's', 't', 0, 0, 0, 2, 0xAA, 0xBB}
	raw = append(raw[:len(raw)], 0x00) // padding byte
	raw = append(raw, trailing...)
	binary.LittleEndian.PutUint64(raw[20:28], uint64(len(raw)-8)) // riffSize64

	d2 := NewDecoder(bytes.NewReader(raw))
	if err := d2.FwdToPCM(); err != nil {
		t.Fatal(err)
	}
	d2.PCMChunk.Drain()
	next, err := d2.NextChunk()
	if err != nil {
		t.Fatal(err)
	}
	if string(next.ID[:]) != "test" {
		t.Fatalf("expected the trailing test chunk after alignment, got %q", next.ID)
	}
	tail := make([]byte, 2)
	if _, err := io.ReadFull(next.R, tail); err != nil {
		t.Fatal(err)
	}
	if tail[0] != 0xAA || tail[1] != 0xBB {
		t.Fatalf("trailing chunk payload mismatch: %v", tail)
	}
}

// TestClassicRIFFSentinelRejected makes sure an overflowed file without an
// RF64 container is not accepted as a legal RIFF.
func TestClassicRIFFSentinelRejected(t *testing.T) {
	buf := new(bytes.Buffer)
	ws := newBufferWriteSeeker(buf)
	e := NewEncoder(ws, 44100, 16, 1, 1)
	if err := e.Write(pcmBuffer(44100, 1, 16, 8)); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	raw := buf.Bytes()
	// Corrupt the data chunk size into the RF64 sentinel while keeping RIFF.
	dataPos := bytes.Index(raw, []byte("data"))
	if dataPos < 0 {
		t.Fatal("data chunk not found")
	}
	binary.LittleEndian.PutUint32(raw[dataPos+4:dataPos+8], rf64SizeSentinel)
	d := NewDecoder(bytes.NewReader(raw))
	err := d.FwdToPCM()
	if !errors.Is(err, ErrRF64BadSentinel) {
		t.Fatalf("expected ErrRF64BadSentinel, got %v", err)
	}
}

// TestRF64JustUnderLimit keeps a 32-bit-fit RF64 file on the real-size path.
func TestRF64JustUnderLimit(t *testing.T) {
	cw := newCountSeekWriter()
	e := NewEncoder(cw, 48000, 16, 2, 1)
	e.RF64 = true
	if err := e.WriteFrame(int16(1)); err != nil {
		t.Fatal(err)
	}
	// Largest frame count whose payload still fits in 32 bits.
	e.frames = math.MaxUint32 / 4
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if got := cw.u32At(t, int64(e.pcmChunkSizePos)); got != uint32(math.MaxUint32-math.MaxUint32%4) {
		t.Fatalf("expected the real 32-bit data size, got 0x%08X", got)
	}
	if got := cw.u64At(t, e.ds64DataSizePos); got != uint64(math.MaxUint32-math.MaxUint32%4) {
		t.Fatalf("ds64 data size mismatch: %d", got)
	}
}

// bufferWriteSeeker is an in-memory io.WriteSeeker used by the header tests.
type bufferWriteSeeker struct {
	buf *bytes.Buffer
	pos int64
}

func newBufferWriteSeeker(buf *bytes.Buffer) *bufferWriteSeeker {
	return &bufferWriteSeeker{buf: buf}
}

func (b *bufferWriteSeeker) Write(p []byte) (int, error) {
	end := b.pos + int64(len(p))
	if int64(b.buf.Len()) < b.pos {
		b.buf.Write(make([]byte, b.pos-int64(b.buf.Len())))
	}
	if end > int64(b.buf.Len()) {
		b.buf.Write(p)
	} else {
		copy(b.buf.Bytes()[b.pos:end], p)
	}
	b.pos = end
	return len(p), nil
}

func (b *bufferWriteSeeker) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		b.pos = offset
	case io.SeekCurrent:
		b.pos += offset
	case io.SeekEnd:
		b.pos = int64(b.buf.Len()) + offset
	}
	return b.pos, nil
}
