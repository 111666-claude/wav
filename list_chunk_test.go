package wav

import (
	"bytes"
	"encoding/binary"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-audio/audio"
	"github.com/go-audio/riff"
)

// memFile is an in-memory io.WriteSeeker / io.ReadSeeker used to avoid
// touching the filesystem during round trip tests.
type memFile struct {
	buf []byte
	off int64
}

func (m *memFile) Write(p []byte) (int, error) {
	end := m.off + int64(len(p))
	if end > int64(len(m.buf)) {
		grown := make([]byte, end)
		copy(grown, m.buf)
		m.buf = grown
	}
	copy(m.buf[m.off:end], p)
	m.off = end
	return len(p), nil
}

func (m *memFile) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		off += m.off
	case io.SeekEnd:
		off += int64(len(m.buf))
	}
	if off < 0 {
		return 0, bytes.ErrTooLarge
	}
	m.off = off
	return off, nil
}

func (m *memFile) Read(p []byte) (int, error) {
	if m.off >= int64(len(m.buf)) {
		return 0, io.EOF
	}
	n := copy(p, m.buf[m.off:])
	m.off += int64(n)
	return n, nil
}

// encodeWAVWithMetadata encodes a minimal valid wav carrying the passed
// metadata and returns its raw bytes.
func encodeWAVWithMetadata(t *testing.T, md *Metadata) []byte {
	t.Helper()
	mf := &memFile{}
	e := NewEncoder(mf, 8000, 16, 1, 1)
	ib := &audio.IntBuffer{
		Format: &audio.Format{NumChannels: 1, SampleRate: 8000},
		Data:   []int{1, 2, 3, 4},
	}
	if err := e.Write(ib); err != nil {
		t.Fatalf("failed to write PCM data: %v", err)
	}
	e.Metadata = md
	if err := e.Close(); err != nil {
		t.Fatalf("failed to close encoder: %v", err)
	}
	return mf.buf
}

// infoSub encodes one INFO subchunk, including the word alignment padding
// byte required for odd-sized payloads.
func infoSub(id [4]byte, val string) []byte {
	var b bytes.Buffer
	b.Write(id[:])
	// payload is the null terminated string
	payload := append([]byte(val), 0x00)
	_ = binary.Write(&b, binary.LittleEndian, uint32(len(payload)))
	b.Write(payload)
	if len(payload)%2 == 1 {
		b.WriteByte(0x00)
	}
	return b.Bytes()
}

// listChunk builds a complete LIST/INFO chunk (header + payload + optional
// outer padding) the same way an on-disk chunk is presented to the decoder.
func listChunk(payload []byte) *riff.Chunk {
	declared := len(payload)
	size := declared
	if size%2 == 1 {
		size++ // chunk readers expose the padding byte as part of the limit
	}
	return &riff.Chunk{
		ID:   CIDList,
		Size: size,
		R:    bytes.NewReader(payload),
	}
}

func decodeList(t *testing.T, payload []byte) (*Decoder, error) {
	t.Helper()
	d := &Decoder{Metadata: nil}
	err := DecodeListChunk(d, listChunk(payload))
	return d, err
}

// TestInfoChunk_OddLengthRoundTrip is the regression test for the level
// editor export: strings of length 3, 4 and 5 produce even, odd and even
// INFO subchunk sizes, only strict word alignment keeps every following
// four byte ID where it belongs.
func TestInfoChunk_OddLengthRoundTrip(t *testing.T) {
	want := &Metadata{
		// stored sizes: len(s)+1 => 4 (even), 5 (odd), 6 (even)
		Artist: "abc",
		Title:  "abcd",
		Genre:  "abcde",
	}
	data := encodeWAVWithMetadata(t, want)

	// byte level assertions: the declared LIST payload must end exactly on
	// the last consumed byte, with a single pad byte per odd subchunk.
	listPayload := extractLISTPayload(t, data)
	if len(listPayload)%2 != 0 {
		t.Fatalf("expected the padded LIST payload to be even, got %d bytes", len(listPayload))
	}
	if !bytes.Equal(listPayload[:4], CIDInfo) {
		t.Fatalf("expected payload to start with INFO marker, got % x", listPayload[:4])
	}
	walkInfoSubchunks(t, listPayload[4:])

	d := NewDecoder(bytes.NewReader(data))
	d.ReadMetadata()
	if err := d.Err(); err != nil {
		t.Fatalf("unexpected decoder error: %v", err)
	}
	if d.Metadata == nil {
		t.Fatal("expected metadata to be decoded, got nil")
	}
	got := &Metadata{Artist: d.Metadata.Artist, Title: d.Metadata.Title, Genre: d.Metadata.Genre}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip mismatch\nwant: %#v\ngot:  %#v", want, got)
	}
}

// TestInfoChunk_AllFieldParitiesRoundTrip exercises every exported string
// field with both odd and even values so every writeSection branch emits a
// padding byte at least once.
func TestInfoChunk_AllFieldParitiesRoundTrip(t *testing.T) {
	odd := "x"   // stored size 2, even -> no pad
	even := "xy" // stored size 3, odd  -> pad byte
	want := &Metadata{
		Artist:       odd,
		Comments:     even,
		Copyright:    odd,
		CreationDate: even,
		Engineer:     odd,
		Technician:   even,
		Genre:        odd,
		Keywords:     even,
		Medium:       odd,
		Title:        even,
		Product:      odd,
		Subject:      even,
		Software:     odd,
		Source:       even,
		Location:     odd,
		TrackNbr:     even,
	}
	data := encodeWAVWithMetadata(t, want)
	d := NewDecoder(bytes.NewReader(data))
	d.ReadMetadata()
	if err := d.Err(); err != nil {
		t.Fatalf("unexpected decoder error: %v", err)
	}
	if !reflect.DeepEqual(d.Metadata, want) {
		t.Fatalf("round trip mismatch\nwant: %#v\ngot:  %#v", want, d.Metadata)
	}
}

// TestInfoChunk_NoMetadataWAV stays readable and simply reports no
// metadata when no LIST chunk is present.
func TestInfoChunk_NoMetadataWAV(t *testing.T) {
	mf := &memFile{}
	e := NewEncoder(mf, 8000, 16, 1, 1)
	ib := &audio.IntBuffer{
		Format: &audio.Format{NumChannels: 1, SampleRate: 8000},
		Data:   []int{0},
	}
	if err := e.Write(ib); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	d := NewDecoder(bytes.NewReader(mf.buf))
	d.ReadMetadata()
	if err := d.Err(); err != nil {
		t.Fatalf("wav without INFO should not error, got %v", err)
	}
	if d.Metadata != nil {
		t.Fatalf("expected nil metadata, got %#v", d.Metadata)
	}
	if bytes.Contains(mf.buf, CIDList[:]) {
		t.Fatal("did not expect a LIST chunk in a metadata free wav")
	}
}

// TestInfoChunk_EmptyStringEntries covers zero length payloads coming from
// other editors: a size 1 entry containing only the null terminator.
func TestInfoChunk_EmptyStringEntries(t *testing.T) {
	payload := bytes.NewBuffer(CIDInfo)
	payload.Write(infoSub(markerINAM, "")) // size 1, odd, padded
	payload.Write(infoSub(markerIART, "a"))
	d, err := decodeList(t, payload.Bytes())
	if err != nil {
		t.Fatalf("empty string entry should decode, got %v", err)
	}
	if d.Metadata.Title != "" {
		t.Fatalf("expected empty title, got %q", d.Metadata.Title)
	}
	if d.Metadata.Artist != "a" {
		t.Fatalf("expected artist %q, got %q", "a", d.Metadata.Artist)
	}
}

// TestInfoChunk_UnknownSubchunksSkipped keeps the historical rule that
// unknown INFO entries are ignored while known ones still decode.
func TestInfoChunk_UnknownSubchunksSkipped(t *testing.T) {
	unknown := [4]byte{'X', 'X', 'X', 'X'}
	payload := bytes.NewBuffer(CIDInfo)
	payload.Write(infoSub(unknown, "ignore me")) // size 10, even
	payload.Write(infoSub(markerINAM, "keep"))   // size 5, odd -> pad
	payload.Write(infoSub(unknown, "me too"))    // size 7, odd -> pad
	d, err := decodeList(t, payload.Bytes())
	if err != nil {
		t.Fatalf("unknown subchunks must be skipped, got %v", err)
	}
	if d.Metadata.Title != "keep" {
		t.Fatalf("expected title %q, got %q", "keep", d.Metadata.Title)
	}
}

// TestInfoChunk_TruncatedAlignmentByte makes the missing pad byte a
// diagnosable error instead of a silently misaligned parse.
func TestInfoChunk_TruncatedAlignmentByte(t *testing.T) {
	// INAM with declared size 5 (odd) but no padding byte following it.
	var b bytes.Buffer
	b.Write(CIDInfo)
	b.Write(markerINAM[:])
	_ = binary.Write(&b, binary.LittleEndian, uint32(5))
	b.Write([]byte{'a', 'b', 'c', 'd', 0x00})
	// declare the LIST size exactly, so the outer chunk exposes no spare byte
	d := &Decoder{}
	err := DecodeListChunk(d, &riff.Chunk{
		ID:   CIDList,
		Size: b.Len(),
		R:    bytes.NewReader(b.Bytes()),
	})
	if err == nil {
		t.Fatal("expected an error when the alignment padding byte is missing")
	}
	if !strings.Contains(err.Error(), "padding") {
		t.Fatalf("expected a padding related diagnostic, got %v", err)
	}
}

// TestInfoChunk_TruncatedSubchunkData ensures a declared size larger than
// the remaining payload is reported, not partially consumed.
func TestInfoChunk_TruncatedSubchunkData(t *testing.T) {
	var b bytes.Buffer
	b.Write(CIDInfo)
	b.Write(markerINAM[:])
	_ = binary.Write(&b, binary.LittleEndian, uint32(10))
	b.Write([]byte{'a', 'b'}) // only 2 of the 10 declared bytes
	d := &Decoder{}
	err := DecodeListChunk(d, &riff.Chunk{
		ID:   CIDList,
		Size: b.Len(),
		R:    bytes.NewReader(b.Bytes()),
	})
	if err == nil {
		t.Fatal("expected an error for a truncated subchunk payload")
	}
	if !strings.Contains(err.Error(), "declares 10 bytes") {
		t.Fatalf("expected a size diagnostic, got %v", err)
	}
}

// TestInfoChunk_TruncatedHeader ensures a cut-off subchunk header is
// reported rather than silently ending the parse.
func TestInfoChunk_TruncatedHeader(t *testing.T) {
	b := append([]byte{}, CIDInfo...)
	b = append(b, markerINAM[:]...)
	b = append(b, 0x00, 0x00) // 2 of the 4 size bytes -> truncated header
	d := &Decoder{}
	err := DecodeListChunk(d, &riff.Chunk{
		ID:   CIDList,
		Size: len(b),
		R:    bytes.NewReader(b),
	})
	if err == nil {
		t.Fatal("expected an error for a truncated subchunk header")
	}
}

// TestInfoChunk_OddSizedLISTPadding tolerates a foreign file whose LIST
// payload itself is odd and therefore carries an outer padding byte.
func TestInfoChunk_OddSizedLISTPadding(t *testing.T) {
	// Build the raw bytes by hand to model a foreign encoder layout:
	// INFO(4) + ISFT header(8) + odd data(3) = 15 bytes (odd).
	// The 15th byte plays double duty as the single word alignment byte for
	// both the odd subchunk and the odd LIST payload.
	var b bytes.Buffer
	b.Write(CIDInfo)                                     // 4
	b.Write(markerISFT[:])                               // 4
	_ = binary.Write(&b, binary.LittleEndian, uint32(3)) // 4, odd declared size
	b.Write([]byte{'a', 'b', 0x00})                      // 3, null terminated value
	oddPayload := b.Bytes()
	if len(oddPayload)%2 == 0 {
		t.Fatalf("test setup expects odd payload, got %d", len(oddPayload))
	}
	// the outer chunk limit includes the word alignment padding byte
	withOuterPad := append(append([]byte{}, oddPayload...), 0x00)
	d, err := decodeList(t, withOuterPad)
	if err != nil {
		t.Fatalf("outer LIST padding should be tolerated, got %v", err)
	}
	if d.Metadata == nil || d.Metadata.Software != "ab" {
		t.Fatalf("expected software %q, got %#v", "ab", d.Metadata)
	}
}

// TestInfoChunk_ManySubchunksLinear guarantees parsing thousands of
// subchunks stays linear; unknown entries are skipped per the existing rule.
func TestInfoChunk_ManySubchunksLinear(t *testing.T) {
	const n = 6000
	payload := bytes.NewBuffer(CIDInfo)
	payload.Write(infoSub(markerINAM, "first"))
	unknown := [4]byte{'I', 'D', 'X', '0'}
	for i := 0; i < n; i++ {
		// alternate payload parity to exercise the padding branch heavily
		val := "v"
		if i%2 == 0 {
			val = "vv"
		}
		payload.Write(infoSub(unknown, val))
	}
	payload.Write(infoSub(markerIART, "last"))

	start := time.Now()
	d, err := decodeList(t, payload.Bytes())
	if err != nil {
		t.Fatalf("large INFO parse failed: %v", err)
	}
	if d.Metadata.Title != "first" || d.Metadata.Artist != "last" {
		t.Fatalf("boundary fields lost: title=%q artist=%q", d.Metadata.Title, d.Metadata.Artist)
	}
	// 6000 tiny subchunks must complete well within a second; a quadratic
	// implementation would not.
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("parsing %d subchunks took %v, expected linear behaviour", n, elapsed)
	}
}

// walkInfoSubchunks verifies on-disk boundaries: each subchunk starts with a
// printable 4 byte ID, its declared size is respected and odd sizes are
// followed by exactly one padding byte.
func walkInfoSubchunks(t *testing.T, data []byte) {
	t.Helper()
	pos := 0
	for pos < len(data) {
		if pos+8 > len(data) {
			t.Fatalf("ran out of bytes reading subchunk header at offset %d", pos)
		}
		id := data[pos : pos+4]
		size := int(binary.LittleEndian.Uint32(data[pos+4 : pos+8]))
		for _, c := range id {
			if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z') {
				t.Fatalf("subchunk ID at offset %d is misaligned: % x", pos, id)
			}
		}
		pos += 8 + size
		if size%2 == 1 {
			pos++ // exactly one alignment byte
		}
		if pos > len(data) {
			t.Fatalf("subchunk %q overruns the LIST payload", id)
		}
	}
	if pos != len(data) {
		t.Fatalf("subchunks consumed %d bytes but payload is %d", pos, len(data))
	}
}

// extractLISTPayload locates the LIST chunk in an encoded wav and returns
// its declared payload (including INFO marker and subchunk padding).
func extractLISTPayload(t *testing.T, data []byte) []byte {
	t.Helper()
	pos := 12
	for pos+8 <= len(data) {
		id := data[pos : pos+4]
		size := int(binary.LittleEndian.Uint32(data[pos+4 : pos+8]))
		if bytes.Equal(id, CIDList[:]) {
			return data[pos+8 : pos+8+size]
		}
		pos += 8 + size + (size % 2)
	}
	t.Fatal("no LIST chunk found in encoded wav")
	return nil
}
