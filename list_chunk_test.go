package wav

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// wavWithList builds an in-memory WAV file containing a single fmt chunk,
// the provided LIST chunk payload (including the "INFO" marker) and an
// empty data chunk. listPayload must already include any padding bytes.
func wavWithList(t *testing.T, listPayload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer

	// RIFF/WAVE header, size gets patched in below.
	buf.Write([]byte("RIFF"))
	riffSizePos := buf.Len()
	binary.Write(&buf, binary.LittleEndian, uint32(0))
	buf.Write([]byte("WAVE"))

	// fmt chunk: PCM mono, 8kHz, 8 bits per sample (16 bytes of data).
	fmtData := make([]byte, 16)
	binary.LittleEndian.PutUint16(fmtData[0:2], 1) // PCM
	binary.LittleEndian.PutUint16(fmtData[2:4], 1) // 1 channel
	binary.LittleEndian.PutUint32(fmtData[4:8], 8000)
	binary.LittleEndian.PutUint32(fmtData[8:12], 8000)
	binary.LittleEndian.PutUint16(fmtData[12:14], 1)
	binary.LittleEndian.PutUint16(fmtData[14:16], 8)
	buf.Write([]byte("fmt "))
	binary.Write(&buf, binary.LittleEndian, uint32(len(fmtData)))
	buf.Write(fmtData)

	// LIST chunk. The declared size excludes the chunk header but includes
	// the INFO marker and every subchunk/padding byte.
	buf.Write(CIDList[:])
	binary.Write(&buf, binary.LittleEndian, uint32(len(listPayload)))
	buf.Write(listPayload)

	// Empty data chunk.
	buf.Write([]byte("data"))
	binary.Write(&buf, binary.LittleEndian, uint32(0))

	// Patch the RIFF size (everything after the RIFF header itself).
	riffSize := uint32(buf.Len() - 8)
	binary.LittleEndian.PutUint32(buf.Bytes()[riffSizePos:], riffSize)

	return buf.Bytes()
}

// infoSubchunk encodes one INFO subchunk. The value gets a trailing NUL;
// an odd payload length gets a padding byte so the next subchunk stays
// word aligned, as required by the RIFF container.
func infoSubchunk(id [4]byte, value string) []byte {
	var buf bytes.Buffer
	buf.Write(id[:])
	payload := append([]byte(value), 0x00)
	binary.Write(&buf, binary.LittleEndian, uint32(len(payload)))
	buf.Write(payload)
	if len(payload)%2 == 1 {
		buf.WriteByte(0x00) // word alignment padding
	}
	return buf.Bytes()
}

// TestDecodeListChunk_InfoWithPadding reproduces a LIST/INFO chunk made of
// odd-length (padded) strings, unknown subchunks and a trailing single
// padding byte. All known fields must be retained in full, unknown fields
// must be skipped and no spurious EOF error may be reported.
func TestDecodeListChunk_InfoWithPadding(t *testing.T) {
	var payload bytes.Buffer
	payload.Write(CIDInfo)

	// Even-length strings produce odd data lengths, so every other entry
	// carries a word-alignment padding byte.
	payload.Write(infoSubchunk(markerINAM, "odd-title!")) // 11 data bytes + pad
	payload.Write(infoSubchunk(markerIART, "ab"))         // 3 data bytes + pad
	payload.Write(infoSubchunk(markerIGNR, "jazz!"))      // 6 data bytes, no pad
	payload.Write(infoSubchunk(markerICMT, "hi"))         // 3 data bytes + pad
	payload.Write(infoSubchunk(markerICRD, "2026-11-14")) // 11 data bytes + pad

	// Unknown subchunks, including an odd sized one, must simply be skipped.
	payload.Write(infoSubchunk([4]byte{'I', 'X', 'X', 'X'}, "skip")) // 5 data + pad
	payload.Write(infoSubchunk([4]byte{'I', 'Y', 'Y', 'Y'}, "pad"))  // 4 data, no pad

	// Another known field after the unknown ones proves alignment held.
	// Its trailing padding byte is the single tail byte of the LIST chunk.
	payload.Write(infoSubchunk(markerISFT, "go-audio")) // 9 data bytes + pad

	data := wavWithList(t, payload.Bytes())
	d := NewDecoder(bytes.NewReader(data))
	d.ReadMetadata()
	if err := d.Err(); err != nil {
		t.Fatalf("unexpected error decoding padded INFO chunk: %v", err)
	}
	if d.Metadata == nil {
		t.Fatal("expected metadata to be decoded, got nil")
	}

	want := &Metadata{
		Title:        "odd-title!",
		Artist:       "ab",
		Genre:        "jazz!",
		Comments:     "hi",
		CreationDate: "2026-11-14",
		Software:     "go-audio",
	}
	got := d.Metadata
	if got.Title != want.Title ||
		got.Artist != want.Artist ||
		got.Genre != want.Genre ||
		got.Comments != want.Comments ||
		got.CreationDate != want.CreationDate ||
		got.Software != want.Software {
		t.Fatalf("metadata mismatch:\nwant %#v\ngot  %#v", want, got)
	}
}

// TestDecodeListChunk_NoInfo makes sure a LIST chunk without an INFO
// marker is ignored without error and leaves metadata untouched.
func TestDecodeListChunk_NoInfo(t *testing.T) {
	payload := append([]byte("adtl"),
		infoSubchunk([4]byte{'l', 'a', 'b', 'l'}, "label")...)
	data := wavWithList(t, payload)

	d := NewDecoder(bytes.NewReader(data))
	d.ReadMetadata()
	if err := d.Err(); err != nil {
		t.Fatalf("unexpected error on non-INFO LIST chunk: %v", err)
	}
	if d.Metadata != nil {
		t.Fatalf("expected no metadata for non-INFO LIST chunk, got %#v", d.Metadata)
	}
}

// TestDecodeListChunk_EmptyList exercises an empty LIST chunk: the missing
// INFO marker read surfaces an EOF-derived error which the decoder keeps
// swallowing, so no metadata and no decoder error are observable.
func TestDecodeListChunk_EmptyList(t *testing.T) {
	data := wavWithList(t, nil)

	d := NewDecoder(bytes.NewReader(data))
	d.ReadMetadata()
	if err := d.Err(); err != nil {
		t.Fatalf("empty LIST chunk must keep its existing (swallowed) error semantics, got: %v", err)
	}
	if d.Metadata != nil {
		t.Fatalf("expected no metadata for an empty LIST chunk, got %#v", d.Metadata)
	}
}

// TestDecodeListChunk_CorruptedLength keeps the existing error semantics
// for a subchunk whose declared length runs past the end of the chunk:
// parsing terminates without surfacing a decoder error.
func TestDecodeListChunk_CorruptedLength(t *testing.T) {
	var payload bytes.Buffer
	payload.Write(CIDInfo)
	payload.Write(markerINAM[:])
	// Declare far more data than the chunk actually contains.
	binary.Write(&payload, binary.LittleEndian, uint32(1<<20))
	payload.Write([]byte{'t', 'i', 't'})

	data := wavWithList(t, payload.Bytes())
	d := NewDecoder(bytes.NewReader(data))
	d.ReadMetadata()
	if err := d.Err(); err != nil {
		t.Fatalf("corrupted length must keep its existing (non-fatal) error semantics, got: %v", err)
	}
}

// TestDecodeListChunk_TailPartialHeader covers a trailing byte run that is
// too short to start another subchunk. The old parser tried to read a full
// 8 byte header there and returned "read sub header: unexpected EOF"; the
// parser must instead stop cleanly once no complete header remains.
func TestDecodeListChunk_TailPartialHeader(t *testing.T) {
	var payload bytes.Buffer
	payload.Write(CIDInfo)
	payload.Write(infoSubchunk(markerINAM, "ab")) // odd data + padding byte
	payload.Write([]byte{'I', 'X'})               // 2 trailing bytes, not a header

	data := wavWithList(t, payload.Bytes())
	d := NewDecoder(bytes.NewReader(data))
	d.ReadMetadata()
	if err := d.Err(); err != nil {
		t.Fatalf("a partial trailing header must not report an EOF error, got: %v", err)
	}
	if d.Metadata == nil || d.Metadata.Title != "ab" {
		t.Fatalf("expected title %q to be retained, got %#v", "ab", d.Metadata)
	}
}

// TestDecodeListChunk_NoTruncation makes sure a padding byte following an
// odd sized entry does not shift the next entry, which previously truncated
// every subsequent field.
func TestDecodeListChunk_NoTruncation(t *testing.T) {
	var payload bytes.Buffer
	payload.Write(CIDInfo)
	payload.Write(infoSubchunk(markerINAM, "ab"))   // 3 data bytes + pad
	payload.Write(infoSubchunk(markerIART, "wxyz")) // 5 data bytes + pad

	data := wavWithList(t, payload.Bytes())
	d := NewDecoder(bytes.NewReader(data))
	d.ReadMetadata()
	if err := d.Err(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Metadata == nil {
		t.Fatal("expected metadata, got nil")
	}
	if d.Metadata.Title != "ab" {
		t.Errorf("expected title %q, got %q", "ab", d.Metadata.Title)
	}
	if d.Metadata.Artist != "wxyz" {
		t.Errorf("expected artist %q to be kept in full, got %q", "wxyz", d.Metadata.Artist)
	}
}

// TestDecodeListChunk_Linear makes sure parsing a large number of
// subchunks stays linear: 6000 subchunks must decode without repeated
// whole-buffer copies or quadratic growth.
func TestDecodeListChunk_Linear(t *testing.T) {
	const n = 6000
	var payload bytes.Buffer
	payload.Write(CIDInfo)
	// Alternate between two distinct odd/even sizes so a capacity-based
	// scratch buffer would thrash between growth and shrinkage.
	for i := 0; i < n; i++ {
		if i%2 == 0 {
			payload.Write(infoSubchunk(markerIKEY, "abcd")) // 5 data + pad
		} else {
			payload.Write(infoSubchunk(markerIKEY, "abc")) // 4 data, no pad
		}
	}

	data := wavWithList(t, payload.Bytes())
	d := NewDecoder(bytes.NewReader(data))
	d.ReadMetadata()
	if err := d.Err(); err != nil {
		t.Fatalf("unexpected error decoding %d subchunks: %v", n, err)
	}
	if d.Metadata == nil || d.Metadata.Keywords != "abc" {
		t.Fatalf("expected last keyword value to be retained, got %#v", d.Metadata)
	}
}
