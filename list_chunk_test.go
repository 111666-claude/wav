package wav

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/go-audio/audio"
	"github.com/go-audio/riff"
)

// memFile is an in-memory io.WriteSeeker/io.ReadSeeker used to build WAV
// containers without touching the disk.
type memFile struct {
	data []byte
	pos  int64
}

func (m *memFile) Write(p []byte) (int, error) {
	end := m.pos + int64(len(p))
	if end > int64(len(m.data)) {
		m.data = append(m.data, make([]byte, end-int64(len(m.data)))...)
	}
	copy(m.data[m.pos:end], p)
	m.pos = end
	return len(p), nil
}

func (m *memFile) Read(p []byte) (int, error) {
	if m.pos >= int64(len(m.data)) {
		return 0, io.EOF
	}
	n := copy(p, m.data[m.pos:])
	m.pos += int64(n)
	return n, nil
}

func (m *memFile) Seek(offset int64, whence int) (int64, error) {
	var np int64
	switch whence {
	case io.SeekStart:
		np = offset
	case io.SeekCurrent:
		np = m.pos + offset
	case io.SeekEnd:
		np = int64(len(m.data)) + offset
	default:
		return 0, errors.New("unknown whence")
	}
	if np < 0 {
		return 0, errors.New("negative seek position")
	}
	m.pos = np
	return np, nil
}

// encodeWAV builds a complete WAV file in memory using the public Encoder API
// and attaches the passed metadata.
func encodeWAV(t *testing.T, md *Metadata) *memFile {
	t.Helper()
	out := &memFile{}
	e := NewEncoder(out, 8000, 16, 1, 1)
	buf := &audio.IntBuffer{
		Format: &audio.Format{NumChannels: 1, SampleRate: 8000},
		Data:   []int{0, 1, -1, 2, -2},
	}
	if err := e.Write(buf); err != nil {
		t.Fatalf("failed to write PCM data: %v", err)
	}
	e.Metadata = md
	if err := e.Close(); err != nil {
		t.Fatalf("failed to close the encoder: %v", err)
	}
	return out
}

func decodeMetadata(t *testing.T, data []byte) *Decoder {
	t.Helper()
	d := NewDecoder(bytes.NewReader(data))
	d.ReadMetadata()
	if err := d.Err(); err != nil {
		t.Fatalf("unexpected decoder error: %v", err)
	}
	return d
}

// buildListBody assembles the payload of a LIST chunk (the bytes following the
// 8 byte chunk header). The INFO form type is always written first and each
// entry is laid out exactly like a spec compliant writer would: 4 byte ID,
// 4 byte little endian declared size, data and, when the size is odd, a
// single zero word alignment padding byte.
func buildListBody(entries ...struct {
	id   [4]byte
	data []byte
}) []byte {
	buf := bytes.NewBuffer(nil)
	buf.Write(CIDInfo[:])
	for _, e := range entries {
		buf.Write(e.id[:])
		var size [4]byte
		binary.LittleEndian.PutUint32(size[:], uint32(len(e.data)))
		buf.Write(size[:])
		buf.Write(e.data)
		if len(e.data)%2 == 1 {
			buf.WriteByte(0x00)
		}
	}
	return buf.Bytes()
}

// wrapInWAV wraps a LIST chunk payload into a minimal valid WAV container.
func wrapInWAV(listBody []byte) []byte {
	out := bytes.NewBuffer(nil)
	// fmt chunk (16 bytes of PCM data)
	fmtBody := []byte{
		0x01, 0x00, // PCM
		0x01, 0x00, // 1 channel
		0x40, 0x1f, 0x00, 0x00, // 8000 Hz
		0x80, 0x3e, 0x00, 0x00, // avg bytes/sec
		0x02, 0x00, // block align
		0x10, 0x00, // 16 bits
	}
	pcm := []byte{0, 0, 1, 0}
	riffSize := 4 + (8 + len(fmtBody)) + (8 + len(pcm)) + (8 + len(listBody)) + (len(listBody) & 1)
	out.Write(riff.RiffID[:])
	binary.Write(out, binary.LittleEndian, uint32(riffSize))
	out.Write(riff.WavFormatID[:])
	out.Write(riff.FmtID[:])
	binary.Write(out, binary.LittleEndian, uint32(len(fmtBody)))
	out.Write(fmtBody)
	out.Write(riff.DataFormatID[:])
	binary.Write(out, binary.LittleEndian, uint32(len(pcm)))
	out.Write(pcm)
	out.Write(CIDList[:])
	binary.Write(out, binary.LittleEndian, uint32(len(listBody)))
	out.Write(listBody)
	if len(listBody)%2 == 1 {
		out.WriteByte(0x00)
	}
	return out.Bytes()
}

// TestEncoderDecoder_OddLengthRoundTrip is the regression anchor: strings of
// lengths 3, 4 and 5 (and their parity permutations) must all come back byte
// for byte identical. An odd sized INFO entry only consumes one alignment
// byte.
func TestEncoderDecoder_OddLengthRoundTrip(t *testing.T) {
	testCases := []struct {
		name string
		md   *Metadata
	}{
		{
			name: "string lengths 3,4,5",
			md: &Metadata{
				Artist:   "abc",   // entry size 4, even
				Comments: "abcd",  // entry size 5, odd -> 1 pad byte
				Title:    "abcde", // entry size 6, even
			},
		},
		{
			name: "entry sizes 3,4,5",
			md: &Metadata{
				Artist:   "ab",   // entry size 3, odd -> 1 pad byte
				Comments: "abc",  // entry size 4, even
				Title:    "abcd", // entry size 5, odd -> 1 pad byte
			},
		},
		{
			name: "all parity combinations",
			md: &Metadata{
				Artist:       "a",                // size 2 even
				Comments:     "ab",               // size 3 odd
				Copyright:    "abc",              // size 4 even
				CreationDate: "abcd",             // size 5 odd
				Engineer:     "abcde",            // size 6 even
				Technician:   "abcdef",           // size 7 odd
				Genre:        "abcdefg",          // size 8 even
				Keywords:     "abcdefgh",         // size 9 odd
				Medium:       "abcdefghi",        // size 10 even
				Title:        "abcdefghij",       // size 11 odd
				Product:      "abcdefghijk",      // size 12 even
				Subject:      "abcdefghijkl",     // size 13 odd
				Software:     "abcdefghijklm",    // size 14 even
				Source:       "abcdefghijklmn",   // size 15 odd
				Location:     "abcdefghijklmno",  // size 16 even
				TrackNbr:     "abcdefghijklmnop", // size 17 odd
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			file := encodeWAV(t, tc.md)
			d := decodeMetadata(t, file.data)
			if d.Metadata == nil {
				t.Fatal("expected metadata to be decoded, got nil")
			}
			got := d.Metadata
			if got.Artist != tc.md.Artist {
				t.Errorf("Artist: expected %q, got %q", tc.md.Artist, got.Artist)
			}
			if got.Comments != tc.md.Comments {
				t.Errorf("Comments: expected %q, got %q", tc.md.Comments, got.Comments)
			}
			if got.Copyright != tc.md.Copyright {
				t.Errorf("Copyright: expected %q, got %q", tc.md.Copyright, got.Copyright)
			}
			if got.CreationDate != tc.md.CreationDate {
				t.Errorf("CreationDate: expected %q, got %q", tc.md.CreationDate, got.CreationDate)
			}
			if got.Engineer != tc.md.Engineer {
				t.Errorf("Engineer: expected %q, got %q", tc.md.Engineer, got.Engineer)
			}
			if got.Technician != tc.md.Technician {
				t.Errorf("Technician: expected %q, got %q", tc.md.Technician, got.Technician)
			}
			if got.Genre != tc.md.Genre {
				t.Errorf("Genre: expected %q, got %q", tc.md.Genre, got.Genre)
			}
			if got.Keywords != tc.md.Keywords {
				t.Errorf("Keywords: expected %q, got %q", tc.md.Keywords, got.Keywords)
			}
			if got.Medium != tc.md.Medium {
				t.Errorf("Medium: expected %q, got %q", tc.md.Medium, got.Medium)
			}
			if got.Title != tc.md.Title {
				t.Errorf("Title: expected %q, got %q", tc.md.Title, got.Title)
			}
			if got.Product != tc.md.Product {
				t.Errorf("Product: expected %q, got %q", tc.md.Product, got.Product)
			}
			if got.Subject != tc.md.Subject {
				t.Errorf("Subject: expected %q, got %q", tc.md.Subject, got.Subject)
			}
			if got.Software != tc.md.Software {
				t.Errorf("Software: expected %q, got %q", tc.md.Software, got.Software)
			}
			if got.Source != tc.md.Source {
				t.Errorf("Source: expected %q, got %q", tc.md.Source, got.Source)
			}
			if got.Location != tc.md.Location {
				t.Errorf("Location: expected %q, got %q", tc.md.Location, got.Location)
			}
			if got.TrackNbr != tc.md.TrackNbr {
				t.Errorf("TrackNbr: expected %q, got %q", tc.md.TrackNbr, got.TrackNbr)
			}
		})
	}
}

// TestEncoder_INFOByteLayout verifies the on-disk contract: an odd sized entry
// is followed by exactly one zero padding byte, the declared entry size does
// not include the pad and the LIST chunk ends precisely at its declared size.
func TestEncoder_INFOByteLayout(t *testing.T) {
	md := &Metadata{
		Artist:   "ab",   // "ab\0" -> size 3 (odd), 1 pad byte
		Comments: "abc",  // "abc\0" -> size 4 (even), no pad
		Title:    "abcd", // "abcd\0" -> size 5 (odd), 1 pad byte
	}
	file := encodeWAV(t, md)

	// walk the container chunks and find LIST
	var listOff, listSize int64 = -1, 0
	r := bytes.NewReader(file.data)
	if _, err := r.Seek(12, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	for r.Len() >= 8 {
		var id [4]byte
		if _, err := io.ReadFull(r, id[:]); err != nil {
			t.Fatal(err)
		}
		var sizeB [4]byte
		if _, err := io.ReadFull(r, sizeB[:]); err != nil {
			t.Fatal(err)
		}
		size := int64(binary.LittleEndian.Uint32(sizeB[:]))
		off, _ := r.Seek(0, io.SeekCurrent)
		if id == CIDList {
			listOff, listSize = off, size
		}
		if _, err := r.Seek(size+(size&1), io.SeekCurrent); err != nil {
			t.Fatal(err)
		}
	}
	if listOff < 0 {
		t.Fatal("no LIST chunk found in the encoded WAV")
	}

	body := file.data[listOff : listOff+listSize]
	if !bytes.Equal(body[:4], CIDInfo[:]) {
		t.Fatalf("expected INFO form type, got % x", body[:4])
	}
	pos := 4
	type expect struct {
		id      [4]byte
		size    uint32
		padded  bool
		payload string
	}
	want := []expect{
		{markerIART, 3, true, "ab\x00"},
		{markerICMT, 4, false, "abc\x00"},
		{markerINAM, 5, true, "abcd\x00"},
	}
	for _, w := range want {
		if pos+8 > len(body) {
			t.Fatalf("entry %s header runs past the LIST chunk boundary", w.id)
		}
		gotID := [4]byte{}
		copy(gotID[:], body[pos:pos+4])
		gotSize := binary.LittleEndian.Uint32(body[pos+4 : pos+8])
		if gotID != w.id {
			t.Fatalf("expected entry ID %s, got %s", w.id, gotID)
		}
		if gotSize != w.size {
			t.Fatalf("entry %s: expected declared size %d, got %d", w.id, w.size, gotSize)
		}
		payload := body[pos+8 : pos+8+int(gotSize)]
		if string(payload) != w.payload {
			t.Fatalf("entry %s: expected payload %q, got %q", w.id, w.payload, payload)
		}
		entryEnd := pos + 8 + int(gotSize)
		if w.padded {
			if entryEnd >= len(body) || body[entryEnd] != 0x00 {
				t.Fatalf("entry %s: expected a zero word alignment padding byte", w.id)
			}
			entryEnd++
		}
		pos = entryEnd
	}
	// the chunk must end exactly at its declared size
	if int64(pos) != listSize {
		t.Fatalf("LIST chunk boundary mismatch: consumed %d bytes but declared size is %d", pos, listSize)
	}
	// and no outer padding is needed because an INFO body is always even
	if listSize%2 != 0 {
		t.Fatalf("INFO LIST body should always be even sized, got %d", listSize)
	}
}

// TestDecoder_ExternalOddLengthINFO proves a spec compliant file produced by
// another tool (odd entries with pad bytes) is decoded without drift.
func TestDecoder_ExternalOddLengthINFO(t *testing.T) {
	body := buildListBody(
		struct {
			id   [4]byte
			data []byte
		}{markerINAM, []byte("abc\x00")}, // size 4 even
		struct {
			id   [4]byte
			data []byte
		}{markerIART, []byte("ab\x00")}, // size 3 odd + pad
		struct {
			id   [4]byte
			data []byte
		}{markerISFT, []byte("abcdefgh\x00")}, // size 9 odd + pad
		struct {
			id   [4]byte
			data []byte
		}{markerIPRD, []byte("abcd\x00")}, // size 5 odd + pad
		struct {
			id   [4]byte
			data []byte
		}{markerICMT, []byte("abcde\x00")}, // size 6 even
	)
	d := decodeMetadata(t, wrapInWAV(body))
	if d.Metadata == nil {
		t.Fatal("expected metadata, got nil")
	}
	want := &Metadata{
		Title:    "abc",
		Artist:   "ab",
		Software: "abcdefgh",
		Product:  "abcd",
		Comments: "abcde",
	}
	if d.Metadata.Title != want.Title {
		t.Errorf("Title: expected %q, got %q", want.Title, d.Metadata.Title)
	}
	if d.Metadata.Artist != want.Artist {
		t.Errorf("Artist: expected %q, got %q", want.Artist, d.Metadata.Artist)
	}
	if d.Metadata.Software != want.Software {
		t.Errorf("Software: expected %q, got %q", want.Software, d.Metadata.Software)
	}
	if d.Metadata.Product != want.Product {
		t.Errorf("Product: expected %q, got %q", want.Product, d.Metadata.Product)
	}
	if d.Metadata.Comments != want.Comments {
		t.Errorf("Comments: expected %q, got %q", want.Comments, d.Metadata.Comments)
	}
}

// TestDecoder_FixtureWithOddINFOEntry covers the shipped Sound Forge fixture
// whose IENG entry has an odd declared size.
func TestDecoder_FixtureWithOddINFOEntry(t *testing.T) {
	f, err := os.Open("fixtures/listChunkInHeader.wav")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	d := NewDecoder(f)
	d.ReadMetadata()
	if err := d.Err(); err != nil {
		t.Fatalf("unexpected decoder error: %v", err)
	}
	if d.Metadata == nil {
		t.Fatal("expected metadata, got nil")
	}
	if d.Metadata.CreationDate != "2017-11-21" {
		t.Errorf("CreationDate: expected %q, got %q", "2017-11-21", d.Metadata.CreationDate)
	}
	if d.Metadata.Engineer != "SAMIH 1" {
		t.Errorf("Engineer: expected %q, got %q", "SAMIH 1", d.Metadata.Engineer)
	}
	if d.Metadata.Software != "Sony Sound Forge 8.0" {
		t.Errorf("Software: expected %q, got %q", "Sony Sound Forge 8.0", d.Metadata.Software)
	}
}

// TestDecoder_EmptyStringEntries checks zero and single NUL entries decode as
// empty strings, including when they sit next to odd sized entries.
func TestDecoder_EmptyStringEntries(t *testing.T) {
	body := buildListBody(
		struct {
			id   [4]byte
			data []byte
		}{markerINAM, []byte{}}, // size 0, no pad byte
		struct {
			id   [4]byte
			data []byte
		}{markerIART, []byte{0x00}}, // size 1 odd + pad
		struct {
			id   [4]byte
			data []byte
		}{markerICMT, []byte("note\x00")}, // size 5 odd + pad
	)
	d := decodeMetadata(t, wrapInWAV(body))
	if d.Metadata == nil {
		t.Fatal("expected metadata, got nil")
	}
	if d.Metadata.Title != "" {
		t.Errorf("Title: expected empty string, got %q", d.Metadata.Title)
	}
	if d.Metadata.Artist != "" {
		t.Errorf("Artist: expected empty string, got %q", d.Metadata.Artist)
	}
	if d.Metadata.Comments != "note" {
		t.Errorf("Comments: expected %q, got %q", "note", d.Metadata.Comments)
	}
}

// TestDecoder_NoINFOMetadata keeps the diagnostic behavior for WAV files that
// simply don't carry a LIST/INFO chunk: no error and nil metadata.
func TestDecoder_NoINFOMetadata(t *testing.T) {
	file := encodeWAV(t, nil)
	d := NewDecoder(bytes.NewReader(file.data))
	d.ReadMetadata()
	if err := d.Err(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Metadata != nil {
		t.Fatalf("expected nil metadata, got %#v", d.Metadata)
	}
}

// TestDecoder_TruncatedEntryData ensures an entry whose declared size runs past
// the chunk boundary is reported with a clear error instead of drifting.
func TestDecoder_TruncatedEntryData(t *testing.T) {
	buf := bytes.NewBuffer(nil)
	buf.Write(CIDInfo[:])
	buf.Write(markerINAM[:])
	binary.Write(buf, binary.LittleEndian, uint32(100)) // claims 100 bytes
	buf.Write([]byte("abc"))                            // only 3 available
	body := buf.Bytes()

	ch := &riff.Chunk{
		ID:   CIDList,
		Size: len(body),
		R:    io.LimitReader(bytes.NewReader(body), int64(len(body))),
	}
	d := &Decoder{}
	err := DecodeListChunk(d, ch)
	if err == nil {
		t.Fatal("expected an error for a truncated INFO entry, got nil")
	}
}

// TestDecoder_TruncatedPaddingByte ensures an odd sized entry missing its word
// alignment padding byte is reported with a diagnostic error.
func TestDecoder_TruncatedPaddingByte(t *testing.T) {
	buf := bytes.NewBuffer(nil)
	buf.Write(CIDInfo[:])
	buf.Write(markerINAM[:])
	binary.Write(buf, binary.LittleEndian, uint32(3)) // odd size
	buf.Write([]byte("ab\x00"))                       // no padding byte follows
	body := buf.Bytes()
	if len(body)%2 != 1 {
		t.Fatalf("test setup: expected an odd body length, got %d", len(body))
	}

	ch := &riff.Chunk{
		ID:   CIDList,
		Size: len(body),
		R:    io.LimitReader(bytes.NewReader(body), int64(len(body))),
	}
	d := &Decoder{}
	err := DecodeListChunk(d, ch)
	if err == nil {
		t.Fatal("expected an error for the missing padding byte, got nil")
	}
}

// TestDecoder_TruncatedListChunkAtFileLevel makes sure physical truncation of
// a WAV (the LIST body is shorter than its declared size) stays diagnosable
// through the public decoder API.
func TestDecoder_TruncatedListChunkAtFileLevel(t *testing.T) {
	good := wrapInWAV(buildListBody(
		struct {
			id   [4]byte
			data []byte
		}{markerIART, []byte("ab\x00")},
	))
	// locate the LIST header and claim a size larger than what the file holds
	listIdx := bytes.Index(good, CIDList[:])
	if listIdx < 0 {
		t.Fatal("LIST chunk not found")
	}
	corrupt := make([]byte, len(good))
	copy(corrupt, good)
	binary.LittleEndian.PutUint32(corrupt[listIdx+4:listIdx+8], uint32(len(good)-listIdx))

	d := NewDecoder(bytes.NewReader(corrupt))
	d.ReadMetadata()
	if d.Err() == nil {
		t.Fatal("expected a diagnostic error for a truncated LIST chunk, got nil")
	}
}

// TestDecoder_UnknownEntriesAreSkipped keeps the unknown chunk skip rule:
// unrecognized INFO entries are consumed (including their padding byte) without
// error and the following known entry still decodes.
func TestDecoder_UnknownEntriesAreSkipped(t *testing.T) {
	unknown1 := [4]byte{'X', 'X', 'X', 'X'}
	unknown2 := [4]byte{'Z', 'Z', 'Z', 'Z'}
	body := buildListBody(
		struct {
			id   [4]byte
			data []byte
		}{unknown1, []byte("odd\x00")}, // size 4 even
		struct {
			id   [4]byte
			data []byte
		}{unknown2, []byte("od\x00")}, // size 3 odd + pad
		struct {
			id   [4]byte
			data []byte
		}{markerINAM, []byte("title\x00")}, // size 6 even
	)
	d := decodeMetadata(t, wrapInWAV(body))
	if d.Metadata == nil || d.Metadata.Title != "title" {
		t.Fatalf("expected Title %q, got %#v", "title", d.Metadata)
	}
}

// TestDecoder_6000SubchunksLinear ensures a large number of entries is handled
// in linear time and without alignment drift, even when every entry is odd.
func TestDecoder_6000SubchunksLinear(t *testing.T) {
	buf := bytes.NewBuffer(nil)
	buf.Write(CIDInfo[:])
	unknown := [4]byte{'U', 'N', 'K', 'N'}
	for i := 0; i < 6000; i++ {
		buf.Write(unknown[:])
		binary.Write(buf, binary.LittleEndian, uint32(3)) // odd size
		buf.Write([]byte{'o', 'd', 0x00})
		buf.WriteByte(0x00) // word alignment padding
	}
	// known trailing entry proves no drift accumulated across 6000 entries
	buf.Write(markerINAM[:])
	binary.Write(buf, binary.LittleEndian, uint32(6))
	buf.Write([]byte("title\x00"))
	body := buf.Bytes()

	start := time.Now()
	d := decodeMetadata(t, wrapInWAV(body))
	if time.Since(start) > 5*time.Second {
		t.Fatal("decoding 6000 subchunks took too long; expected linear behavior")
	}
	if d.Metadata == nil || d.Metadata.Title != "title" {
		t.Fatalf("expected Title %q after 6000 unknown entries, got %#v", "title", d.Metadata)
	}
}

// TestEncoder_OddLengthFieldsStillRoundTripThroughFullAPI drives the public
// encoder/decoder entry points end to end and asserts every field survives.
func TestEncoder_OddLengthFieldsStillRoundTripThroughFullAPI(t *testing.T) {
	md := &Metadata{Artist: "ab", Title: "abc", Software: "abcdefgh"}
	file := encodeWAV(t, md)
	d := decodeMetadata(t, file.data)
	if d.Metadata == nil {
		t.Fatal("expected metadata, got nil")
	}
	if d.Metadata.Artist != md.Artist || d.Metadata.Title != md.Title || d.Metadata.Software != md.Software {
		t.Fatalf("round trip mismatch: got %#v", d.Metadata)
	}
}
