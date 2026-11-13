package wav

import (
	"bytes"
	"encoding/binary"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-audio/audio"
	"github.com/go-audio/riff"
)

// infoEntry builds a single INFO subchunk. data is the raw payload (usually a
// null terminated string) and its length is the declared chunk size. RIFF
// requires subchunks to be word aligned, so an odd sized payload is followed by
// a single padding byte which is not included in the declared size.
func infoEntry(id string, data []byte) []byte {
	if len(id) != 4 {
		panic("INFO markers must be 4 bytes")
	}
	buf := make([]byte, 0, 8+len(data)+1)
	buf = append(buf, id...)
	size := make([]byte, 4)
	binary.LittleEndian.PutUint32(size, uint32(len(data)))
	buf = append(buf, size...)
	buf = append(buf, data...)
	if len(data)%2 == 1 {
		buf = append(buf, 0)
	}
	return buf
}

// infoEntryStr is a convenience wrapper returning a null terminated payload.
func infoEntryStr(id, val string) []byte {
	return infoEntry(id, append([]byte(val), 0))
}

// wavWithList wraps the LIST payload (starting with INFO or adtl) in a minimal
// PCM wav container. The LIST chunk is appended after the data chunk, as most
// editors do. If pad is false the outer alignment byte of an odd sized LIST
// chunk is omitted, simulating a truncated file.
func wavWithList(list []byte, pad bool) []byte {
	buf := bytes.NewBuffer(nil)
	buf.WriteString("RIFF")
	binary.Write(buf, binary.LittleEndian, uint32(0)) // patched below
	buf.WriteString("WAVE")

	buf.WriteString("fmt ")
	binary.Write(buf, binary.LittleEndian, uint32(16))
	binary.Write(buf, binary.LittleEndian, uint16(1))     // PCM
	binary.Write(buf, binary.LittleEndian, uint16(1))     // mono
	binary.Write(buf, binary.LittleEndian, uint32(8000))  // sample rate
	binary.Write(buf, binary.LittleEndian, uint32(16000)) // avg bytes/sec
	binary.Write(buf, binary.LittleEndian, uint16(2))     // block align
	binary.Write(buf, binary.LittleEndian, uint16(16))    // bits per sample

	buf.WriteString("data")
	binary.Write(buf, binary.LittleEndian, uint32(2))
	binary.Write(buf, binary.LittleEndian, int16(0))

	buf.WriteString("LIST")
	binary.Write(buf, binary.LittleEndian, uint32(len(list)))
	buf.Write(list)
	if len(list)%2 == 1 && pad {
		buf.WriteByte(0)
	}

	out := buf.Bytes()
	binary.LittleEndian.PutUint32(out[4:], uint32(len(out)-8))
	return out
}

func decodeMetadataBytes(t *testing.T, data []byte) (*Decoder, error) {
	t.Helper()
	d := NewDecoder(bytes.NewReader(data))
	d.ReadMetadata()
	return d, d.Err()
}

// TestDecodeListChunkOddSizes reproduces the anchor: entries with declared
// sizes 3, 4 and 5 must each be read back by value. The odd sized entries
// consume exactly one alignment byte so the following subchunk ID stays put.
func TestDecodeListChunkOddSizes(t *testing.T) {
	list := append([]byte{}, CIDInfo...)
	list = append(list, infoEntryStr("INAM", "ab")...)   // size 3, odd
	list = append(list, infoEntryStr("IART", "abc")...)  // size 4, even
	list = append(list, infoEntryStr("IPRD", "abcd")...) // size 5, odd
	list = append(list, infoEntryStr("ICMT", "ab")...)   // size 3, odd, trailing

	if len(list)%2 != 0 {
		t.Fatalf("test INFO payload must be even sized, got %d", len(list))
	}

	d, err := decodeMetadataBytes(t, wavWithList(list, true))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := &Metadata{Title: "ab", Artist: "abc", Product: "abcd", Comments: "ab"}
	if !reflect.DeepEqual(want, d.Metadata) {
		t.Fatalf("expected\n%#v\ngot\n%#v", want, d.Metadata)
	}
}

// TestDecodeListChunkAllMarkers checks that every supported marker keeps its
// value when subchunk sizes alternate between odd and even.
func TestDecodeListChunkAllMarkers(t *testing.T) {
	entries := []struct {
		id  string
		val string
	}{
		{"IARL", "a"},
		{"IART", "bb"},
		{"ISFT", "ccc"},
		{"ICRD", "dddd"},
		{"ICOP", "eeeee"},
		{"INAM", "ffffff"},
		{"IENG", "ggggggg"},
		{"IGNR", "hhhhhhhh"},
		{"IPRD", "iiiiiiiii"},
		{"ISRC", "jjjjjjjjjj"},
		{"ISBJ", "kkkkkkkkkkk"},
		{"ICMT", "llllllllllll"},
		{"ITRK", "13"},
		{"ITCH", "mmmmmmmmmmmmm"},
		{"IKEY", "nnnnnnnnnnnnnn"},
		{"IMED", "ooooooooooooooo"},
	}
	list := append([]byte{}, CIDInfo...)
	for _, e := range entries {
		list = append(list, infoEntryStr(e.id, e.val)...)
	}

	d, err := decodeMetadataBytes(t, wavWithList(list, true))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := &Metadata{
		Location: "a", Artist: "bb", Software: "ccc", CreationDate: "dddd",
		Copyright: "eeeee", Title: "ffffff", Engineer: "ggggggg", Genre: "hhhhhhhh",
		Product: "iiiiiiiii", Source: "jjjjjjjjjj", Subject: "kkkkkkkkkkk",
		Comments: "llllllllllll", TrackNbr: "13", Technician: "mmmmmmmmmmmmm",
		Keywords: "nnnnnnnnnnnnnn", Medium: "ooooooooooooooo",
	}
	if !reflect.DeepEqual(want, d.Metadata) {
		t.Fatalf("expected\n%#v\ngot\n%#v", want, d.Metadata)
	}
}

// TestDecodeListChunkEmptyStrings verifies that zero sized entries and entries
// containing only a null terminator don't disturb the following subchunks.
func TestDecodeListChunkEmptyStrings(t *testing.T) {
	list := append([]byte{}, CIDInfo...)
	list = append(list, infoEntry("INAM", nil)...)       // declared size 0
	list = append(list, infoEntry("ICMT", []byte{0})...) // single null byte
	list = append(list, infoEntryStr("IART", "ab")...)   // size 3, odd
	list = append(list, infoEntryStr("IPRD", "abcd")...) // size 5, odd
	list = append(list, infoEntryStr("IGNR", "even")...) // size 5, odd

	d, err := decodeMetadataBytes(t, wavWithList(list, true))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := &Metadata{Artist: "ab", Product: "abcd", Genre: "even"}
	if !reflect.DeepEqual(want, d.Metadata) {
		t.Fatalf("expected\n%#v\ngot\n%#v", want, d.Metadata)
	}
}

// TestDecodeListChunkNoInfo checks the unknown subchunk rule is preserved: a
// LIST chunk that isn't an INFO list is skipped without error or metadata.
func TestDecodeListChunkNoInfo(t *testing.T) {
	list := append([]byte{}, []byte("adtl")...)
	list = append(list, infoEntryStr("labl", "hello")...)

	d, err := decodeMetadataBytes(t, wavWithList(list, true))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Metadata != nil {
		t.Fatalf("expected no metadata for a non-INFO LIST chunk, got %#v", d.Metadata)
	}
}

// TestDecodeMetadataWithoutList verifies that a wav without any INFO chunk is
// still decodable and simply has no metadata.
func TestDecodeMetadataWithoutList(t *testing.T) {
	// A minimal wav with only fmt and data chunks.
	data := wavWithList(nil, true)
	// Trim the empty LIST chunk (header only) that wavWithList always appends.
	listOff := bytes.Index(data, []byte("LIST"))
	data = data[:listOff]
	binary.LittleEndian.PutUint32(data[4:], uint32(len(data)-8))

	d, err := decodeMetadataBytes(t, data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Metadata != nil {
		t.Fatalf("expected no metadata for a wav without a LIST chunk, got %#v", d.Metadata)
	}
}

// TestEncodeMetadataEmptyStrings verifies that a metadata struct whose fields
// are all empty writes a valid but empty INFO list and reads back cleanly.
func TestEncodeMetadataEmptyStrings(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "empty-info-*.wav")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	e := NewEncoder(f, 8000, 16, 1, 1)
	buf := &audio.IntBuffer{
		Format: &audio.Format{NumChannels: 1, SampleRate: 8000},
		Data:   []int{0, 0},
	}
	if err := e.Write(buf); err != nil {
		t.Fatal(err)
	}
	e.Metadata = &Metadata{}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	d := NewDecoder(f)
	d.ReadMetadata()
	if err := d.Err(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(&Metadata{}, d.Metadata) {
		t.Fatalf("expected empty metadata, got %#v", d.Metadata)
	}
}

// TestDecodeListChunkUnknownMarkers checks unknown INFO markers are skipped and
// don't break the entries that follow.
func TestDecodeListChunkUnknownMarkers(t *testing.T) {
	list := append([]byte{}, CIDInfo...)
	list = append(list, infoEntryStr("XXXX", "ignored")...) // size 8, even
	list = append(list, infoEntryStr("ZZZZ", "yes")...)     // size 4, even
	list = append(list, infoEntryStr("INAM", "title")...)   // size 6, even
	list = append(list, infoEntryStr("YYYY", "odd")...)     // size 4, even

	d, err := decodeMetadataBytes(t, wavWithList(list, true))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := &Metadata{Title: "title"}
	if !reflect.DeepEqual(want, d.Metadata) {
		t.Fatalf("expected\n%#v\ngot\n%#v", want, d.Metadata)
	}
}

// TestDecodeListChunkCorrupted checks that malformed INFO data produces a
// diagnosable error instead of silent corruption or a panic.
func TestDecodeListChunkCorrupted(t *testing.T) {
	makeList := func(entries ...[]byte) []byte {
		list := append([]byte{}, CIDInfo...)
		for _, e := range entries {
			list = append(list, e...)
		}
		return list
	}

	// A subchunk that declares more data than the LIST chunk can hold.
	oversized := makeList(func() []byte {
		b := []byte("INAM")
		size := make([]byte, 4)
		binary.LittleEndian.PutUint32(size, 100)
		return append(b, size...)
	}())

	// A subchunk header cut short: only 5 of the 8 header bytes are present.
	truncatedHeader := makeList([]byte("INAM\x04\x00"))

	// An odd sized subchunk that claims the following entry's ID as its
	// alignment byte, shifting the remaining entries.
	misaligned := makeList(
		[]byte("INAM\x03\x00\x00\x00ab\x00"),
		infoEntryStr("IART", "artist"),
	)

	testCases := []struct {
		name    string
		list    []byte
		pad     bool
		wantErr string
	}{
		{"oversized subchunk", oversized, true, "declares"},
		{"truncated subchunk header", truncatedHeader, true, "truncated"},
		{"misaligned subchunk", misaligned, true, "declares"},
		// The file ends before the declared LIST size, including its
		// alignment byte.
		{"truncated LIST chunk", misaligned, false, "failed to read the LIST chunk"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := decodeMetadataBytes(t, wavWithList(tc.list, tc.pad))
			if err == nil {
				t.Fatalf("expected an error, decoded metadata: %#v", d.Metadata)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected an error mentioning %q, got: %v", tc.wantErr, err)
			}
		})
	}
}

// TestEncodeInfoChunkAlignment verifies the encoder's on-disk layout: each
// subchunk declares the value length plus its null terminator, odd sized
// entries are followed by exactly one alignment byte, and the LIST chunk ends
// exactly at its declared size.
func TestEncodeInfoChunkAlignment(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "info-alignment-*.wav")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	e := NewEncoder(f, 8000, 16, 1, 1)
	buf := &audio.IntBuffer{
		Format: &audio.Format{NumChannels: 1, SampleRate: 8000},
		Data:   []int{1, 2, 3, 4},
	}
	if err := e.Write(buf); err != nil {
		t.Fatal(err)
	}
	e.Metadata = &Metadata{
		Artist:    "aaa",   // size 4, even
		Comments:  "bbbb",  // size 5, odd -> one pad byte
		Copyright: "ccccc", // size 6, even
		Title:     "dd",    // size 3, odd -> one pad byte
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	if uint32(len(data)-8) != binary.LittleEndian.Uint32(data[4:8]) {
		t.Fatalf("RIFF size %d doesn't match file length %d", binary.LittleEndian.Uint32(data[4:8]), len(data)-8)
	}

	listOff := bytes.Index(data, []byte("LIST"))
	if listOff < 0 {
		t.Fatal("no LIST chunk written")
	}
	listSize := int(binary.LittleEndian.Uint32(data[listOff+4 : listOff+8]))
	listEnd := listOff + 8 + listSize
	if listEnd > len(data) {
		t.Fatalf("LIST chunk declares %d bytes, only %d available", listSize, len(data)-(listOff+8))
	}
	if got := string(data[listOff+8 : listOff+12]); got != "INFO" {
		t.Fatalf("expected INFO subchunk, got %q", got)
	}

	pos := listOff + 12 // skip LIST header + INFO
	var subchunks int
	for pos < listEnd {
		if pos+8 > listEnd {
			t.Fatalf("subchunk header at %d overruns the declared LIST end %d", pos, listEnd)
		}
		id := string(data[pos : pos+4])
		size := int(int32(binary.LittleEndian.Uint32(data[pos+4 : pos+8])))
		if size <= 0 {
			t.Fatalf("%s has an invalid size %d", id, size)
		}
		if pos+8+size > listEnd {
			t.Fatalf("%s at %d declares %d bytes and overruns the LIST end %d", id, pos, size, listEnd)
		}
		payload := data[pos+8 : pos+8+size]
		if payload[len(payload)-1] != 0 {
			t.Fatalf("%s payload isn't null terminated: %q", id, payload)
		}
		if size%2 == 1 {
			// Exactly one alignment byte must follow, still inside the LIST.
			padPos := pos + 8 + size
			if padPos+1 > listEnd {
				t.Fatalf("odd sized %s of size %d has no alignment byte inside the LIST", id, size)
			}
			if data[padPos] != 0 {
				t.Fatalf("expected a zero alignment byte after %s, got %#x", id, data[padPos])
			}
			pos = padPos + 1
		} else {
			pos += 8 + size
		}
		subchunks++
	}
	if pos != listEnd {
		t.Fatalf("LIST parsing stopped at %d but the chunk declares its end at %d", pos, listEnd)
	}
	if subchunks != 4 {
		t.Fatalf("expected 4 INFO subchunks, got %d", subchunks)
	}
	// The INFO payload is always even sized, so the LIST chunk must not
	// require an outer alignment byte.
	if listSize%2 != 0 {
		t.Fatalf("expected an even LIST size, got %d", listSize)
	}
}

// TestEncoderMetadataRoundTripOddSizes writes odd and even length values of
// lengths 3, 4 and 5 through the encoder and verifies every field reads back
// unchanged.
func TestEncoderMetadataRoundTripOddSizes(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "roundtrip-*.wav")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	e := NewEncoder(f, 8000, 16, 1, 1)
	buf := &audio.IntBuffer{
		Format: &audio.Format{NumChannels: 1, SampleRate: 8000},
		Data:   []int{0, 1, 0, -1},
	}
	if err := e.Write(buf); err != nil {
		t.Fatal(err)
	}
	want := &Metadata{
		Artist:    "aaa",
		Comments:  "bbbb",
		Copyright: "ccccc",
		Title:     "dddddd",
		Genre:     "ggggggg",
		TrackNbr:  "42",
	}
	e.Metadata = want
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	d := NewDecoder(f)
	d.ReadMetadata()
	if err := d.Err(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(want, d.Metadata) {
		t.Fatalf("expected\n%#v\ngot\n%#v", want, d.Metadata)
	}
}

// TestDecodeListChunkManySubchunks makes sure a large INFO list is handled in
// linear time and doesn't lose or shift entries.
func TestDecodeListChunkManySubchunks(t *testing.T) {
	const count = 6000
	list := make([]byte, 0, 4+count*12)
	list = append(list, CIDInfo...)
	for i := 0; i < count; i++ {
		val := "v" + string(rune('a'+i%26))
		if i%2 == 0 {
			val += "x" // odd declared size
		}
		list = append(list, infoEntryStr("ICMT", val)...)
	}
	data := wavWithList(list, true)

	start := time.Now()
	d, err := decodeMetadataBytes(t, data)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Metadata == nil {
		t.Fatal("expected metadata to be decoded")
	}
	last := "v" + string(rune('a'+(count-1)%26))
	if (count-1)%2 == 0 {
		last += "x"
	}
	if d.Metadata.Comments != last {
		t.Fatalf("expected the last comment to be %q, got %q", last, d.Metadata.Comments)
	}
	// A quadratic implementation would take orders of magnitude longer; this
	// generous bound only guards against pathological blowups.
	if elapsed > 10*time.Second {
		t.Fatalf("decoding %d subchunks took %s, expected linear behavior", count, elapsed)
	}
}

// TestDecodeListChunkBoundaries exercises the chunk boundary handling directly,
// bypassing the RIFF parser's size rounding so the exact declared LIST sizes
// are preserved.
func TestDecodeListChunkBoundaries(t *testing.T) {
	oddEntryNoPad := append(append([]byte{}, CIDInfo...), []byte("INAM\x03\x00\x00\x00ab\x00")...)
	entries := append(append([]byte{}, CIDInfo...), infoEntryStr("IART", "artist")...)
	entries = append(entries, infoEntryStr("INAM", "title")...)
	// Since every entry is word aligned, an INFO payload is always even sized;
	// append one byte to model an enclosing odd sized LIST chunk.
	entriesWithOuterPad := append(append([]byte{}, entries...), 0)
	// Same entries with their last byte chopped off, so the final subchunk
	// can't be satisfied by the declared LIST size.
	truncated := entries[:len(entries)-1]

	testCases := []struct {
		name    string
		payload []byte
		want    *Metadata
		wantErr bool
	}{
		{"INFO only", append([]byte{}, CIDInfo...), &Metadata{}, false},
		{"INFO plus outer pad byte", append(append([]byte{}, CIDInfo...), 0), &Metadata{}, false},
		{"odd entry without alignment byte", oddEntryNoPad, nil, true},
		{"entries then outer pad byte", entriesWithOuterPad, &Metadata{Artist: "artist", Title: "title"}, false},
		{"truncated chunk payload", truncated, nil, true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			d := &Decoder{}
			ch := &riff.Chunk{ID: CIDList, Size: len(tc.payload), R: bytes.NewReader(tc.payload)}
			err := DecodeListChunk(d, ch)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got metadata %#v", d.Metadata)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(tc.want, d.Metadata) {
				t.Fatalf("expected\n%#v\ngot\n%#v", tc.want, d.Metadata)
			}
			if !ch.IsFullyRead() {
				t.Fatalf("expected the chunk to be fully consumed, pos %d of %d", ch.Pos, ch.Size)
			}
		})
	}
}

// TestDecodeListChunkNilInputs preserves the existing error behavior for nil
// arguments.
func TestDecodeListChunkNilInputs(t *testing.T) {
	if err := DecodeListChunk(&Decoder{}, nil); err == nil {
		t.Fatal("expected an error for a nil chunk")
	}
	ch := &riff.Chunk{ID: CIDList, Size: 0, R: bytes.NewReader(nil)}
	if err := DecodeListChunk(nil, ch); err == nil {
		t.Fatal("expected an error for a nil decoder")
	}
}
