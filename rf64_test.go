package wav

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/go-audio/audio"
)

// memWriteSeeker is an in-memory io.WriteSeeker used for exact byte level
// assertions.
type memWriteSeeker struct {
	data []byte
	pos  int64
}

func (m *memWriteSeeker) Write(p []byte) (int, error) {
	if m.pos < 0 {
		return 0, errors.New("negative position")
	}
	end := m.pos + int64(len(p))
	if end > int64(len(m.data)) {
		m.data = append(m.data, make([]byte, end-int64(len(m.data)))...)
	}
	n := copy(m.data[m.pos:], p)
	m.pos += int64(n)
	return n, nil
}

func (m *memWriteSeeker) Seek(offset int64, whence int) (int64, error) {
	var pos int64
	switch whence {
	case io.SeekStart:
		pos = offset
	case io.SeekCurrent:
		pos = m.pos + offset
	case io.SeekEnd:
		pos = int64(len(m.data)) + offset
	default:
		return 0, errors.New("invalid whence")
	}
	if pos < 0 {
		return 0, errors.New("negative position")
	}
	m.pos = pos
	return pos, nil
}

// sparseWriteSeeker simulates a writer of arbitrary size without allocating
// memory for the data. Only the first keep bytes are recorded, and the logical
// size grows with every write/seek past the end. This is how the >4 GiB
// length arithmetic is verified without allocating a matching buffer.
type sparseWriteSeeker struct {
	data  []byte
	keep  int64
	pos   int64
	size  int64
	limit int64 // when >= 0, writes at or past this offset fail
}

func newSparseWriteSeeker(keep int64) *sparseWriteSeeker {
	return &sparseWriteSeeker{keep: keep, limit: -1}
}

func (s *sparseWriteSeeker) Write(p []byte) (int, error) {
	if s.limit >= 0 && s.pos >= s.limit {
		return 0, fmt.Errorf("%w at offset %d", errInjectedWrite, s.pos)
	}
	if s.pos < s.keep {
		upTo := int64(len(p))
		if s.pos+upTo > s.keep {
			upTo = s.keep - s.pos
		}
		if int64(len(s.data)) < s.pos+upTo {
			s.data = append(s.data, make([]byte, s.pos+upTo-int64(len(s.data)))...)
		}
		copy(s.data[s.pos:], p[:upTo])
	}
	s.pos += int64(len(p))
	if s.pos > s.size {
		s.size = s.pos
	}
	return len(p), nil
}

func (s *sparseWriteSeeker) Seek(offset int64, whence int) (int64, error) {
	var pos int64
	switch whence {
	case io.SeekStart:
		pos = offset
	case io.SeekCurrent:
		pos = s.pos + offset
	case io.SeekEnd:
		pos = s.size + offset
	default:
		return 0, errors.New("invalid whence")
	}
	if pos < 0 {
		return 0, errors.New("negative position")
	}
	// Seeking beyond the end doesn't allocate; a later write extends size.
	s.pos = pos
	if pos > s.size {
		s.size = pos
	}
	return pos, nil
}

func (s *sparseWriteSeeker) head(n int64) []byte {
	if n > int64(len(s.data)) {
		n = int64(len(s.data))
	}
	return s.data[:n]
}

// unseekableWriter exposes only sequential writes.
type unseekableWriter struct {
	buf bytes.Buffer
}

func (u *unseekableWriter) Write(p []byte) (int, error) { return u.buf.Write(p) }

func (u *unseekableWriter) Seek(offset int64, whence int) (int64, error) {
	return 0, errors.New("seek not supported")
}

func intBuffer(sampleRate, channels int, data ...int) *audio.IntBuffer {
	return &audio.IntBuffer{
		Format: &audio.Format{SampleRate: sampleRate, NumChannels: channels},
		Data:   data,
	}
}

// rf64Header mirrors the parsed fixed part of an RF64 file header.
type rf64Header struct {
	riffSize    uint64
	dataSize    uint64
	sampleCount uint64
	tableLen    uint32
	fmtChunk    []byte
	dataOffset  int64
}

// parseRF64Header validates the fixed RF64 header layout and returns the
// parsed fields. It expects no chunks between ds64 and fmt other than fmt.
func parseRF64Header(t *testing.T, b []byte) rf64Header {
	t.Helper()
	if len(b) < 80 {
		t.Fatalf("file too short for an RF64 header: %d bytes", len(b))
	}
	if got := string(b[0:4]); got != "RF64" {
		t.Fatalf("form type = %q, want RF64", got)
	}
	if got := binary.LittleEndian.Uint32(b[4:8]); got != 0xFFFFFFFF {
		t.Fatalf("32-bit RIFF size = %#x, want 0xFFFFFFFF placeholder", got)
	}
	if got := string(b[8:12]); got != "WAVE" {
		t.Fatalf("form = %q, want WAVE", got)
	}
	if got := string(b[12:16]); got != "ds64" {
		t.Fatalf("first chunk = %q, want ds64", got)
	}
	if got := binary.LittleEndian.Uint32(b[16:20]); got != ds64ChunkSize {
		t.Fatalf("ds64 chunk size = %d, want %d", got, ds64ChunkSize)
	}
	h := rf64Header{
		riffSize:    binary.LittleEndian.Uint64(b[20:28]),
		dataSize:    binary.LittleEndian.Uint64(b[28:36]),
		sampleCount: binary.LittleEndian.Uint64(b[36:44]),
		tableLen:    binary.LittleEndian.Uint32(b[44:48]),
	}
	if h.tableLen != 0 {
		t.Fatalf("ds64 table length = %d, want 0", h.tableLen)
	}
	if got := string(b[48:52]); got != "fmt " {
		t.Fatalf("chunk after ds64 = %q, want \"fmt \"", got)
	}
	fmtSize := binary.LittleEndian.Uint32(b[52:56])
	if fmtSize != 16 {
		t.Fatalf("fmt chunk size = %d, want 16", fmtSize)
	}
	h.fmtChunk = append([]byte(nil), b[56:72]...)
	if got := string(b[72:76]); got != "data" {
		t.Fatalf("chunk after fmt = %q, want data", got)
	}
	if got := binary.LittleEndian.Uint32(b[76:80]); got != 0xFFFFFFFF {
		t.Fatalf("32-bit data size = %#x, want 0xFFFFFFFF placeholder", got)
	}
	h.dataOffset = 80
	return h
}

func assertRF64Sizes(t *testing.T, h rf64Header, frames, dataBytes, fileSize int64) {
	t.Helper()
	if h.dataSize != uint64(dataBytes) {
		t.Errorf("ds64 dataSize = %d, want %d", h.dataSize, dataBytes)
	}
	if h.sampleCount != uint64(frames) {
		t.Errorf("ds64 sampleCount = %d, want %d frames", h.sampleCount, frames)
	}
	// riffSize covers everything after the initial 8 bytes of the form.
	if h.riffSize != uint64(fileSize-8) {
		t.Errorf("ds64 riffSize = %d, want fileSize-8 = %d", h.riffSize, fileSize-8)
	}
}

func TestEncoderDefaultRIFFIsByteCompatible(t *testing.T) {
	var w memWriteSeeker
	e := NewEncoder(&w, 8000, 16, 1, 1)
	if err := e.Write(intBuffer(8000, 1, 1, 2)); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	// Expected classic RIFF output for two 16-bit mono frames.
	want := []byte("RIFF")
	want = appendU32(want, 40) // file size - 8
	want = append(want, []byte("WAVEfmt ")...)
	want = appendU32(want, 16)
	want = appendU16(want, 1)     // PCM
	want = appendU16(want, 1)     // channels
	want = appendU32(want, 8000)  // sample rate
	want = appendU32(want, 16000) // avg bytes/sec
	want = appendU16(want, 2)     // block align
	want = appendU16(want, 16)    // bits
	want = append(want, []byte("data")...)
	want = appendU32(want, 4)
	want = appendU16(want, 1)
	want = appendU16(want, 2)

	if !bytes.Equal(w.data, want) {
		t.Fatalf("default RIFF output changed:\n got: %x\nwant: %x", w.data, want)
	}
	if !e.RF64 {
		if got := string(w.data[:4]); got != "RIFF" {
			t.Fatalf("default form = %q, want RIFF", got)
		}
	}
}

func appendU32(b []byte, v uint32) []byte {
	var tmp [4]byte
	binary.LittleEndian.PutUint32(tmp[:], v)
	return append(b, tmp[:]...)
}

func appendU16(b []byte, v uint16) []byte {
	var tmp [2]byte
	binary.LittleEndian.PutUint16(tmp[:], v)
	return append(b, tmp[:]...)
}

func TestRF64SmallFileHeader(t *testing.T) {
	for _, tc := range []struct {
		name      string
		bitDepth  int
		channels  int
		data      []int
		frames    int64
		blockAlgn uint16
	}{
		{"16-bit mono 4 frames", 16, 1, []int{1, 2, 3, 4}, 4, 2},
		{"16-bit stereo 3 frames", 16, 2, []int{1, 2, 3, 4, 5, 6}, 3, 4},
		{"24-bit mono 2 frames", 24, 1, []int{1, 2}, 2, 3},
		{"8-bit mono odd frames", 8, 1, []int{1, 2, 3}, 3, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var w memWriteSeeker
			e := NewRF64Encoder(&w, 8000, tc.bitDepth, tc.channels, 1)
			if err := e.Write(intBuffer(8000, tc.channels, tc.data...)); err != nil {
				t.Fatal(err)
			}
			if err := e.Close(); err != nil {
				t.Fatal(err)
			}

			h := parseRF64Header(t, w.data)
			dataBytes := int64(tc.frames) * int64(tc.blockAlgn)
			assertRF64Sizes(t, h, tc.frames, dataBytes, int64(len(w.data)))

			if got := binary.LittleEndian.Uint16(h.fmtChunk[12:14]); got != tc.blockAlgn {
				t.Errorf("fmt block align = %d, want %d", got, tc.blockAlgn)
			}
			if got := binary.LittleEndian.Uint16(h.fmtChunk[14:16]); got != uint16(tc.bitDepth) {
				t.Errorf("fmt bits per sample = %d, want %d", got, tc.bitDepth)
			}

			pcm := w.data[h.dataOffset:]
			padding := int64(0)
			if dataBytes%2 == 1 {
				padding = 1
				if pcm[len(pcm)-1] != 0 {
					t.Errorf("missing zero padding byte after an odd data chunk")
				}
			}
			if int64(len(pcm)) != dataBytes+padding {
				t.Fatalf("data chunk occupies %d bytes, want %d (+%d padding)", len(pcm), dataBytes, padding)
			}
			// dataSize excludes the padding byte.
			if h.dataSize != uint64(dataBytes) {
				t.Errorf("dataSize = %d, want %d", h.dataSize, dataBytes)
			}
		})
	}
}

func TestRF64ExplicitViaOptionAndSetRF64(t *testing.T) {
	var w memWriteSeeker
	e := NewEncoder(&w, 8000, 16, 1, 1, WithRF64())
	if err := e.Write(intBuffer(8000, 1, 7)); err != nil {
		t.Fatal(err)
	}
	if err := e.SetRF64(false); !errors.Is(err, ErrModeLocked) {
		t.Fatalf("SetRF64 after write = %v, want ErrModeLocked", err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	h := parseRF64Header(t, w.data)
	assertRF64Sizes(t, h, 1, 2, int64(len(w.data)))

	// Setting the mode before the first write is allowed.
	var w2 memWriteSeeker
	e2 := NewEncoder(&w2, 8000, 16, 1, 1)
	if err := e2.SetRF64(true); err != nil {
		t.Fatal(err)
	}
	if err := e2.Write(intBuffer(8000, 1, 1)); err != nil {
		t.Fatal(err)
	}
	if err := e2.Close(); err != nil {
		t.Fatal(err)
	}
	if string(w2.data[:4]) != "RF64" {
		t.Fatalf("SetRF64(true) produced %q, want RF64", w2.data[:4])
	}
}

func TestRF64ModeLockedViaField(t *testing.T) {
	var w memWriteSeeker
	e := NewRF64Encoder(&w, 8000, 16, 1, 1)
	if err := e.Write(intBuffer(8000, 1, 1)); err != nil {
		t.Fatal(err)
	}
	// Mutating the public field after the header is written is rejected.
	e.RF64 = false
	if err := e.Write(intBuffer(8000, 1, 2)); !errors.Is(err, ErrModeLocked) {
		t.Fatalf("Write after mode change = %v, want ErrModeLocked", err)
	}
	if err := e.Close(); !errors.Is(err, ErrModeLocked) {
		t.Fatalf("Close after mode change = %v, want ErrModeLocked", err)
	}
}

func TestRF64WithMetadata(t *testing.T) {
	var w memWriteSeeker
	e := NewRF64Encoder(&w, 8000, 8, 1, 1)
	e.Metadata = &Metadata{Title: "rf64 title", Artist: "someone"}
	// 3 frames of 8-bit mono => odd data chunk that needs a pad byte before
	// the LIST chunk.
	if err := e.Write(intBuffer(8000, 1, 1, 2, 3)); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	h := parseRF64Header(t, w.data)
	dataBytes := int64(3)
	assertRF64Sizes(t, h, 3, dataBytes, int64(len(w.data)))

	// data + padding byte, then the LIST chunk.
	if got := string(w.data[h.dataOffset+dataBytes+1 : h.dataOffset+dataBytes+5]); got != "LIST" {
		t.Fatalf("chunk after padded data = %q, want LIST", got)
	}
	listSize := int64(binary.LittleEndian.Uint32(w.data[h.dataOffset+dataBytes+5:]))
	listTotal := 8 + listSize
	if listTotal%2 == 1 {
		listTotal++
	}
	if got, want := int64(len(w.data)), h.dataOffset+dataBytes+1+listTotal; got != want {
		t.Fatalf("file size = %d, want %d", got, want)
	}
	if h.riffSize != uint64(len(w.data)-8) {
		t.Fatalf("riffSize = %d, want %d", h.riffSize, len(w.data)-8)
	}
}

func TestRF64LargeSparseWriter(t *testing.T) {
	const (
		sampleRate = 48000
		bitDepth   = 16
		channels   = 2
	)
	blockAlign := int64(bitDepth / 8 * channels)

	w := newSparseWriteSeeker(256)
	e := NewRF64Encoder(w, sampleRate, bitDepth, channels, 1)
	if err := e.Write(intBuffer(sampleRate, channels, 0, 0, 1, 1)); err != nil {
		t.Fatal(err)
	}

	// Simulate a multi-hour recording: more than 4 GiB of PCM while only a
	// handful of bytes were physically written. The sparse writer tracks the
	// logical size, so no matching allocation is needed.
	frames := int64(1)<<32/blockAlign + 12345
	e.frames = frames

	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	h := parseRF64Header(t, w.head(80))
	dataBytes := frames * blockAlign
	if dataBytes <= (int64(1) << 32) {
		t.Fatalf("test data size %d should exceed 4 GiB", dataBytes)
	}
	assertRF64Sizes(t, h, frames, dataBytes, w.size)

	// The file must be addressed by its logical size, beyond 4 GiB.
	if w.size < dataBytes {
		t.Fatalf("logical file size %d is smaller than the data size %d", w.size, dataBytes)
	}
	if got, want := w.size, int64(80)+dataBytes; got != want {
		t.Fatalf("logical file size = %d, want %d", got, want)
	}
}

func TestRF64LargeOddDataSizePadding(t *testing.T) {
	// 8-bit mono so an odd frame count yields an odd data chunk.
	w := newSparseWriteSeeker(256)
	e := NewRF64Encoder(w, 8000, 8, 1, 1)
	if err := e.Write(intBuffer(8000, 1, 0)); err != nil {
		t.Fatal(err)
	}
	frames := int64(1)<<32 + 1 // odd, > 4 GiB
	e.frames = frames
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	h := parseRF64Header(t, w.head(80))
	dataBytes := frames
	assertRF64Sizes(t, h, frames, dataBytes, w.size)
	if got, want := w.size, int64(80)+dataBytes+1; got != want {
		t.Fatalf("logical file size = %d, want %d (data + pad)", got, want)
	}
	if h.dataSize != uint64(frames) {
		t.Fatalf("dataSize = %d, want %d (padding excluded)", h.dataSize, frames)
	}
}

func TestRIFFOverflowIsRejected(t *testing.T) {
	w := newSparseWriteSeeker(256)
	e := NewEncoder(w, 8000, 16, 1, 1)
	if err := e.Write(intBuffer(8000, 1, 1)); err != nil {
		t.Fatal(err)
	}
	// Put the frame counter one frame short of the 4 GiB data limit.
	e.frames = int64(maxUint32Value) / 2
	if err := e.Write(intBuffer(8000, 1, 1)); !errors.Is(err, ErrSizeOverflow) {
		t.Fatalf("Write past the RIFF limit = %v, want ErrSizeOverflow", err)
	}
	// The failed write must not be reported as a successful file.
	if err := e.Close(); !errors.Is(err, ErrSizeOverflow) {
		t.Fatalf("Close after an overflow = %v, want ErrSizeOverflow", err)
	}

	// RF64 accepts the same frame count.
	w2 := newSparseWriteSeeker(256)
	e2 := NewRF64Encoder(w2, 8000, 16, 1, 1)
	if err := e2.Write(intBuffer(8000, 1, 1)); err != nil {
		t.Fatal(err)
	}
	e2.frames = int64(maxUint32Value) / 2
	if err := e2.Write(intBuffer(8000, 1, 1)); err != nil {
		t.Fatalf("RF64 write past the RIFF limit failed: %v", err)
	}
	if err := e2.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestFrameCounterOverflow(t *testing.T) {
	w := newSparseWriteSeeker(256)
	e := NewRF64Encoder(w, 8000, 16, 1, 1)
	if err := e.Write(intBuffer(8000, 1, 1)); err != nil {
		t.Fatal(err)
	}
	e.frames = maxInt64Value
	if err := e.WriteFrame(uint16(0)); !errors.Is(err, ErrSizeOverflow) {
		t.Fatalf("WriteFrame with an exhausted counter = %v, want ErrSizeOverflow", err)
	}
	if err := e.Close(); !errors.Is(err, ErrSizeOverflow) {
		t.Fatalf("Close after a counter overflow = %v, want ErrSizeOverflow", err)
	}

	// The size helper also refuses a frame count that would overflow int64.
	e2 := NewRF64Encoder(newSparseWriteSeeker(64), 8000, 16, 1, 1)
	e2.frames = maxInt64Value/2 + 1
	if _, err := e2.dataBytes(); !errors.Is(err, ErrSizeOverflow) {
		t.Fatalf("dataBytes with an overflowing count = %v, want ErrSizeOverflow", err)
	}
}

func TestUnseekableWriter(t *testing.T) {
	for _, newE := range []func(io.WriteSeeker) *Encoder{
		func(w io.WriteSeeker) *Encoder { return NewEncoder(w, 8000, 16, 1, 1) },
		func(w io.WriteSeeker) *Encoder { return NewRF64Encoder(w, 8000, 16, 1, 1) },
	} {
		uw := &unseekableWriter{}
		e := newE(uw)
		err := e.Write(intBuffer(8000, 1, 1, 2))
		if !errors.Is(err, ErrUnseekable) {
			t.Fatalf("Write to an unseekable writer = %v, want ErrUnseekable", err)
		}
		if uw.buf.Len() != 0 {
			t.Fatalf("unseekable writer received %d bytes before the failure", uw.buf.Len())
		}
		if err := e.Close(); !errors.Is(err, ErrUnseekable) {
			t.Fatalf("Close after an unseekable writer = %v, want ErrUnseekable", err)
		}
	}
}

func TestWriteInterruptionIsNotReportedAsSuccess(t *testing.T) {
	w := newSparseWriteSeeker(256)
	// Let the header through, then fail the PCM write.
	w.limit = 44
	e := NewEncoder(w, 8000, 16, 1, 1)
	err := e.Write(intBuffer(8000, 1, 1, 2))
	if err == nil {
		t.Fatal("Write succeeded despite an injected failure")
	}
	if !errors.Is(err, errInjectedWrite) {
		t.Fatalf("Write error = %v, want %v", err, errInjectedWrite)
	}
	// The error is sticky: later writes fail the same way.
	if err := e.Write(intBuffer(8000, 1, 1)); !errors.Is(err, errInjectedWrite) {
		t.Fatalf("second Write = %v, want %v", err, errInjectedWrite)
	}
	// And Close must not report the partial file as a success.
	if err := e.Close(); !errors.Is(err, errInjectedWrite) {
		t.Fatalf("Close after a write failure = %v, want the write error", err)
	}
	if w.size >= 44+4 {
		t.Fatalf("the failing writer unexpectedly stored %d bytes", w.size)
	}
}

var errInjectedWrite = errors.New("injected write failure")

func TestCloseIdempotent(t *testing.T) {
	for _, newE := range []func(io.WriteSeeker) *Encoder{
		func(w io.WriteSeeker) *Encoder { return NewEncoder(w, 8000, 16, 1, 1) },
		func(w io.WriteSeeker) *Encoder { return NewRF64Encoder(w, 8000, 16, 1, 1) },
	} {
		w := newSparseWriteSeeker(512)
		e := newE(w)
		e.Metadata = &Metadata{Title: "close me once"}
		if err := e.Write(intBuffer(8000, 1, 1, 2, 3)); err != nil {
			t.Fatal(err)
		}
		if err := e.Close(); err != nil {
			t.Fatal(err)
		}
		sizeAfterFirst := w.size
		first := e.Close()
		second := e.Close()
		if first != nil || second != nil {
			t.Fatalf("repeated Close returned %v/%v, want nil", first, second)
		}
		if w.size != sizeAfterFirst {
			t.Fatalf("repeated Close grew the file from %d to %d bytes", sizeAfterFirst, w.size)
		}
		if err := e.Write(intBuffer(8000, 1, 4)); !errors.Is(err, ErrClosed) {
			t.Fatalf("Write after Close = %v, want ErrClosed", err)
		}
	}
}

func TestCloseWithoutData(t *testing.T) {
	w := newSparseWriteSeeker(64)
	e := NewEncoder(w, 8000, 16, 1, 1)
	if err := e.Close(); !errors.Is(err, ErrNoData) {
		t.Fatalf("Close without data = %v, want ErrNoData", err)
	}
	// Repeated Close keeps returning the same explicit result.
	if err := e.Close(); !errors.Is(err, ErrNoData) {
		t.Fatalf("second Close without data = %v, want ErrNoData", err)
	}
}

func TestCloseRepeatedAfterError(t *testing.T) {
	uw := &unseekableWriter{}
	e := NewEncoder(uw, 8000, 16, 1, 1)
	if err := e.Write(intBuffer(8000, 1, 1)); err == nil {
		t.Fatal("expected the unseekable write to fail")
	}
	first := e.Close()
	second := e.Close()
	if first == nil || second == nil {
		t.Fatalf("repeated Close after an error returned %v/%v, want the same error", first, second)
	}
	if first.Error() != second.Error() {
		t.Fatalf("repeated Close errors differ: %q vs %q", first, second)
	}
}

func TestWriteFrameValidation(t *testing.T) {
	w := newSparseWriteSeeker(512)
	e := NewEncoder(w, 8000, 16, 1, 1)
	// 16-bit mono needs a 2-byte frame.
	if err := e.WriteFrame(uint32(0)); err == nil {
		t.Fatal("WriteFrame accepted a frame of the wrong size")
	}
	if err := e.WriteFrame(uint16(42)); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if w.size != 44+2 {
		t.Fatalf("file size = %d, want 46", w.size)
	}
}

func TestEncoderEmbeddedAtOffset(t *testing.T) {
	for _, rf64 := range []bool{false, true} {
		name := "riff"
		newE := func(w io.WriteSeeker) *Encoder { return NewEncoder(w, 8000, 16, 1, 1) }
		if rf64 {
			name = "rf64"
			newE = func(w io.WriteSeeker) *Encoder { return NewRF64Encoder(w, 8000, 16, 1, 1) }
		}
		t.Run(name, func(t *testing.T) {
			w := newSparseWriteSeeker(1024)
			prefix := []byte("PREFIX!!")
			if _, err := w.Write(prefix); err != nil {
				t.Fatal(err)
			}

			e := newE(w)
			if err := e.Write(intBuffer(8000, 1, 1, 2, 3)); err != nil {
				t.Fatal(err)
			}
			if err := e.Close(); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(w.head(int64(len(prefix))), prefix) {
				t.Fatal("the encoder overwrote the embedded prefix")
			}
			body := w.head(w.size)[len(prefix):]
			if rf64 {
				h := parseRF64Header(t, body)
				assertRF64Sizes(t, h, 3, 6, int64(len(body)))
			} else {
				if got, want := binary.LittleEndian.Uint32(body[4:8]), uint32(len(body)-8); got != want {
					t.Fatalf("embedded RIFF size = %d, want %d", got, want)
				}
				if got := binary.LittleEndian.Uint32(body[40:44]); got != 6 {
					t.Fatalf("embedded data chunk size = %d, want 6", got)
				}
			}
			// The cursor must be left at the end of the container so the caller
			// can keep appending.
			if w.pos != w.size {
				t.Fatalf("cursor at %d, want end of file %d", w.pos, w.size)
			}
		})
	}
}
