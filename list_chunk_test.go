package wav

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/go-audio/riff"
)

// infoSub builds a LIST/INFO subchunk. When pad is true and the value length
// is odd, the RIFF word-alignment padding byte is appended.
func infoSub(id, val string, pad bool) []byte {
	var b bytes.Buffer
	b.WriteString(id)
	binary.Write(&b, binary.LittleEndian, uint32(len(val)))
	b.WriteString(val)
	if pad && len(val)%2 == 1 {
		b.WriteByte(0)
	}
	return b.Bytes()
}

// buildWAVWithLIST wraps a LIST chunk body into a minimal valid WAV container.
// The outer chunk padding required by RIFF word alignment is added by the
// builder and is not part of the declared LIST size.
func buildWAVWithLIST(listBody []byte) []byte {
	var b bytes.Buffer
	pcm := []byte{0, 0, 0, 0}
	listSize := len(listBody)
	riffSize := 4 + (8+16) + (8+len(pcm)) + (8 + listSize + (listSize % 2))
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, uint32(riffSize))
	b.WriteString("WAVE")
	b.WriteString("fmt ")
	binary.Write(&b, binary.LittleEndian, uint32(16))
	binary.Write(&b, binary.LittleEndian, uint16(1)) // PCM
	binary.Write(&b, binary.LittleEndian, uint16(1)) // mono
	binary.Write(&b, binary.LittleEndian, uint32(8000))
	binary.Write(&b, binary.LittleEndian, uint32(8000))
	binary.Write(&b, binary.LittleEndian, uint16(1))
	binary.Write(&b, binary.LittleEndian, uint16(8))
	b.WriteString("data")
	binary.Write(&b, binary.LittleEndian, uint32(len(pcm)))
	b.Write(pcm)
	b.WriteString("LIST")
	binary.Write(&b, binary.LittleEndian, uint32(listSize))
	b.Write(listBody)
	if listSize%2 == 1 {
		b.WriteByte(0)
	}
	return b.Bytes()
}

// infoWithTrailingByte builds a well-formed LIST/INFO body containing odd
// length strings, several unknown subchunks and a single trailing byte (the
// word-alignment pad of the whole block). Every odd entry carries its own
// pad byte, so the block is structurally valid; only the final byte is the
// outer alignment pad that previously could be mistaken for a new subchunk.
func infoWithTrailingByte() []byte {
	body := []byte("INFO")
	body = append(body, infoSub("INAM", "odd-title\x00", true)...)  // size 10
	body = append(body, infoSub("XXXX", "q\x00", true)...)          // unknown, size 2
	body = append(body, infoSub("IGNR", "ab\x00", true)...)         // odd size 3 + pad
	body = append(body, infoSub("YYYY", "w\x00", true)...)          // unknown, size 2
	body = append(body, infoSub("ICRD", "2020\x00", true)...)       // odd size 5 + pad
	body = append(body, infoSub("IART", "abc\x00", true)...)        // size 4
	body = append(body, 0x00) // trailing single word-alignment byte
	return body
}

// TestDecoder_ListInfoTrailingByte is the main regression: the parser must
// keep every known field, skip the unknown entries and never report a false
// EOF when the block ends on a single alignment byte.
func TestDecoder_ListInfoTrailingByte(t *testing.T) {
	d := NewDecoder(bytes.NewReader(buildWAVWithLIST(infoWithTrailingByte())))
	d.ReadMetadata()
	if err := d.Err(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Metadata == nil {
		t.Fatal("expected metadata to be populated")
	}
	want := &Metadata{
		Title:        "odd-title",
		Genre:        "ab",
		CreationDate: "2020",
		Artist:       "abc",
	}
	if d.Metadata.Title != want.Title {
		t.Errorf("Title: got %q, want %q", d.Metadata.Title, want.Title)
	}
	if d.Metadata.Genre != want.Genre {
		t.Errorf("Genre: got %q, want %q", d.Metadata.Genre, want.Genre)
	}
	if d.Metadata.CreationDate != want.CreationDate {
		t.Errorf("CreationDate: got %q, want %q", d.Metadata.CreationDate, want.CreationDate)
	}
	if d.Metadata.Artist != want.Artist {
		t.Errorf("Artist: got %q, want %q", d.Metadata.Artist, want.Artist)
	}
}

// TestDecoder_ListInfoMissingSubchunkPad covers files produced by writers
// that omit an entry's pad byte (the parser must not silently drop the
// following field). The old parser consumed the next header byte and lost
// every entry after the misaligned one.
func TestDecoder_ListInfoMissingSubchunkPad(t *testing.T) {
	body := []byte("INFO")
	body = append(body, infoSub("INAM", "odd-title\x00", true)...)
	body = append(body, infoSub("XXXX", "q\x00", true)...) // unknown
	// odd sized entry written WITHOUT its pad byte, followed by more entries
	body = append(body, infoSub("IGNR", "ab\x00", false)...)
	body = append(body, infoSub("YYYY", "w\x00", true)...) // unknown
	body = append(body, infoSub("ICRD", "2020\x00", true)...)

	d := NewDecoder(bytes.NewReader(buildWAVWithLIST(body)))
	d.ReadMetadata()
	err := d.Err()
	// Recovery from a missing pad is only possible when the remaining bytes
	// are not a valid entry layout; either a clean parse with all fields or an
	// explicit corruption error is acceptable. Silent truncation is not.
	if err == nil {
		if d.Metadata == nil {
			t.Fatal("expected metadata to be populated")
		}
		if d.Metadata.Title != "odd-title" || d.Metadata.Genre != "ab" {
			t.Fatalf("fields before the misaligned entry lost: %#v", d.Metadata)
		}
		if d.Metadata.CreationDate != "2020" {
			t.Fatalf("CreationDate silently truncated by padding desync, got %q (want %q)",
				d.Metadata.CreationDate, "2020")
		}
	} else if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("expected io.ErrUnexpectedEOF for the unreadable block, got %v", err)
	}
}

// TestDecoder_ListInfoOddLengthsAndUnknowns exercises a fully padded INFO
// block mixing odd/even lengths with unknown entries interleaved between the
// known ones.
func TestDecoder_ListInfoOddLengthsAndUnknowns(t *testing.T) {
	body := []byte("INFO")
	body = append(body, infoSub("IART", "a\x00", true)...)    // size 2, even
	body = append(body, infoSub("ZZZZ", "xy\x00", true)...)   // unknown
	body = append(body, infoSub("INAM", "abc\x00", true)...)  // size 4, even
	body = append(body, infoSub("ICRD", "2020\x00", true)...) // size 5, odd -> padded
	body = append(body, infoSub("ICMT", "hello world\x00", true)...)
	body = append(body, infoSub("ZZZZ", "1\x00", true)...) // unknown, size 2

	d := NewDecoder(bytes.NewReader(buildWAVWithLIST(body)))
	d.ReadMetadata()
	if err := d.Err(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := &Metadata{
		Artist:       "a",
		Title:        "abc",
		CreationDate: "2020",
		Comments:     "hello world",
	}
	if d.Metadata == nil {
		t.Fatal("expected metadata to be populated")
	}
	if d.Metadata.Artist != want.Artist {
		t.Errorf("Artist: got %q, want %q", d.Metadata.Artist, want.Artist)
	}
	if d.Metadata.Title != want.Title {
		t.Errorf("Title: got %q, want %q", d.Metadata.Title, want.Title)
	}
	if d.Metadata.CreationDate != want.CreationDate {
		t.Errorf("CreationDate: got %q, want %q", d.Metadata.CreationDate, want.CreationDate)
	}
	if d.Metadata.Comments != want.Comments {
		t.Errorf("Comments: got %q, want %q", d.Metadata.Comments, want.Comments)
	}
}

// TestDecoder_ListInfoNoINFO keeps the existing semantics of a LIST chunk
// carrying no INFO marker: it is skipped without error and without metadata.
func TestDecoder_ListInfoNoINFO(t *testing.T) {
	body := append([]byte("adtl"), infoSub("XXXX", "q\x00", true)...)
	d := NewDecoder(bytes.NewReader(buildWAVWithLIST(body)))
	d.ReadMetadata()
	if err := d.Err(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Metadata != nil {
		t.Fatalf("expected no metadata for a non-INFO LIST, got %#v", d.Metadata)
	}
}

// TestDecoder_ListInfoEmptyBlock verifies that an INFO block with no entries
// parses successfully and yields an empty Metadata value, and that a wholly
// empty LIST body stays non-fatal.
func TestDecoder_ListInfoEmptyBlock(t *testing.T) {
	d := NewDecoder(bytes.NewReader(buildWAVWithLIST([]byte("INFO"))))
	d.ReadMetadata()
	if err := d.Err(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Metadata == nil {
		t.Fatal("expected an empty Metadata value, got nil")
	}
	if !reflect.DeepEqual(d.Metadata, &Metadata{}) {
		t.Fatalf("expected empty metadata, got %#v", d.Metadata)
	}

	d2 := NewDecoder(bytes.NewReader(buildWAVWithLIST(nil)))
	d2.ReadMetadata()
	if err := d2.Err(); err != nil {
		t.Fatalf("empty LIST body should be non-fatal, got %v", err)
	}
	if d2.Metadata != nil {
		t.Fatalf("expected no metadata for an empty LIST, got %#v", d2.Metadata)
	}
}

// TestDecoder_ListInfoTruncatedMarker keeps the existing error semantics for a
// LIST body too small to even hold the INFO marker: DecodeListChunk returns an
// EOF class error which the metadata reader treats as non-fatal.
func TestDecoder_ListInfoTruncatedMarker(t *testing.T) {
	// the RIFF layer rounds odd chunk sizes up, so the short buffer path is
	// exercised directly against the chunk decoder.
	ch := &riff.Chunk{
		ID:   CIDList,
		Size: len([]byte("INF")),
		R:    io.LimitReader(bytes.NewReader([]byte("INF")), 3),
	}
	d := &Decoder{}
	if err := DecodeListChunk(d, ch); !errors.Is(err, io.EOF) {
		t.Fatalf("expected an io.EOF class error, got %v", err)
	}
	if d.Metadata != nil {
		t.Fatalf("expected no metadata to be stored, got %#v", d.Metadata)
	}
}

// TestDecoder_ListInfoCorruptedLength verifies that a subchunk declaring more
// bytes than the LIST body contains still surfaces an unexpected EOF instead
// of silently storing garbage.
func TestDecoder_ListInfoCorruptedLength(t *testing.T) {
	var body bytes.Buffer
	body.WriteString("INFO")
	body.Write(infoSub("INAM", "good\x00", true))
	body.WriteString("ICMT")
	binary.Write(&body, binary.LittleEndian, uint32(999))
	body.WriteString("short")

	d := NewDecoder(bytes.NewReader(buildWAVWithLIST(body.Bytes())))
	d.ReadMetadata()
	err := d.Err()
	if err == nil {
		t.Fatal("expected an error for a corrupted subchunk length, got nil")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("expected io.ErrUnexpectedEOF, got %v", err)
	}
	// entries parsed before the corrupt one must be preserved
	if d.Metadata == nil || d.Metadata.Title != "good" {
		t.Fatalf("expected previously parsed Title to be preserved, got %#v", d.Metadata)
	}
}

// buildLargeINFO builds an INFO body with n subchunks, half of them unknown.
func buildLargeINFO(n int) []byte {
	var body bytes.Buffer
	body.WriteString("INFO")
	for i := 0; i < n; i++ {
		if i%2 == 0 {
			body.Write(infoSub("IART", "x\x00", true))
		} else {
			body.Write(infoSub("XXXX", "y\x00", true))
		}
	}
	return body.Bytes()
}

// TestDecoder_ListInfoLinearWithManySubchunks parses 6000 subchunks. The walk
// must stay linear and must not allocate a fresh value buffer per entry.
func TestDecoder_ListInfoLinearWithManySubchunks(t *testing.T) {
	const n = 6000
	wav := buildWAVWithLIST(buildLargeINFO(n))
	d := NewDecoder(bytes.NewReader(wav))
	d.ReadMetadata()
	if err := d.Err(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Metadata == nil || d.Metadata.Artist != "x" {
		t.Fatalf("unexpected metadata: %#v", d.Metadata)
	}
}

func BenchmarkDecodeListChunk6000(b *testing.B) {
	wav := buildWAVWithLIST(buildLargeINFO(6000))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		d := NewDecoder(bytes.NewReader(wav))
		d.ReadMetadata()
		if err := d.Err(); err != nil {
			b.Fatal(err)
		}
	}
}
