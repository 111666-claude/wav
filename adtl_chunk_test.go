package wav

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"reflect"
	"testing"

	"github.com/go-audio/audio"
)

// memFile is an in memory io.WriteSeeker / io.ReadSeeker used so round trip
// tests never touch the file system.
type memFile struct {
	buf []byte
	pos int64
}

func (m *memFile) Write(p []byte) (int, error) {
	if int64(len(p))+m.pos > int64(len(m.buf)) {
		grown := make([]byte, int64(len(p))+m.pos)
		copy(grown, m.buf)
		m.buf = grown
	}
	n := copy(m.buf[m.pos:], p)
	m.pos += int64(n)
	return n, nil
}

func (m *memFile) Read(p []byte) (int, error) {
	if m.pos >= int64(len(m.buf)) {
		return 0, io.EOF
	}
	n := copy(p, m.buf[m.pos:])
	m.pos += int64(n)
	if n == 0 {
		return 0, io.EOF
	}
	return n, nil
}

func (m *memFile) Seek(offset int64, whence int) (int64, error) {
	var pos int64
	switch whence {
	case 0:
		pos = offset
	case 1:
		pos = m.pos + offset
	case 2:
		pos = int64(len(m.buf)) + offset
	}
	if pos < 0 {
		return 0, errors.New("negative position")
	}
	m.pos = pos
	return pos, nil
}

func idUint32(n uint32) [4]byte {
	return [4]byte{byte(n), byte(n >> 8), byte(n >> 16), byte(n >> 24)}
}

var testDataID = [4]byte{'d', 'a', 't', 'a'}

func testCuePoints() []*CuePoint {
	mk := func(id, offset uint32) *CuePoint {
		return &CuePoint{
			ID:           idUint32(id),
			Position:     offset,
			DataChunkID:  testDataID,
			SampleOffset: offset,
		}
	}
	return []*CuePoint{mk(1, 0), mk(2, 10), mk(3, 40)}
}

func testMetadata() *Metadata {
	return &Metadata{
		Title:     "adtl test",
		CuePoints: testCuePoints(),
		CueLabels: []*CueLabel{
			{CueID: idUint32(1), Text: "Intro"},
			{CueID: idUint32(2), Text: "Verse-A"},
			// odd length text: "Coda!" (5) + NUL = 6 even; use "Coda" below for odd cases
			{CueID: idUint32(3), Text: "Coda"},
		},
		CueNotes: []*CueNote{
			// 12 UTF-8 bytes + NUL = 13, odd, requires padding
			{CueID: idUint32(1), Text: "多轨注释"},
			// "peak" (4) + NUL = 5, odd, requires padding
			{CueID: idUint32(2), Text: "peak"},
		},
		CueSegments: []*CueSegment{
			{
				CueID:        idUint32(3),
				SampleLength: 40, // starts at frame 40, 80 frames total -> exact fit
				Purpose:      [4]byte{'r', 'g', 'n', ' '},
				CodePage:     65001,
				Text:         "fade out",
			},
		},
	}
}

func encodeTestWAV(t *testing.T, md *Metadata) *memFile {
	t.Helper()
	out := &memFile{}
	e := NewEncoder(out, 8000, 16, 1, 1)
	buf := &audio.IntBuffer{
		Format: &audio.Format{NumChannels: 1, SampleRate: 8000},
		Data:   make([]int, 80),
	}
	for i := range buf.Data {
		buf.Data[i] = i * 128
	}
	if err := e.Write(buf); err != nil {
		t.Fatalf("write: %v", err)
	}
	e.Metadata = md
	if err := e.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return out
}

// TestAdtlRoundTrip writes a WAV with labels, notes and segments (mixing even
// and odd length UTF-8 strings) and checks every field comes back identical,
// preserving order.
func TestAdtlRoundTrip(t *testing.T) {
	md := testMetadata()
	out := encodeTestWAV(t, md)

	d := NewDecoder(bytes.NewReader(out.buf))
	d.ReadMetadata()
	if err := d.Err(); err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	got := d.Metadata
	if got == nil {
		t.Fatal("no metadata decoded")
	}
	if got.Title != "adtl test" {
		t.Errorf("Title = %q, want %q", got.Title, "adtl test")
	}
	if !reflect.DeepEqual(got.CuePoints, md.CuePoints) {
		t.Fatalf("cue points mismatch:\n got %#v\nwant %#v", got.CuePoints, md.CuePoints)
	}
	if !reflect.DeepEqual(got.CueLabels, md.CueLabels) {
		t.Fatalf("labels mismatch:\n got %#v\nwant %#v", got.CueLabels, md.CueLabels)
	}
	if !reflect.DeepEqual(got.CueNotes, md.CueNotes) {
		t.Fatalf("notes mismatch:\n got %#v\nwant %#v", got.CueNotes, md.CueNotes)
	}
	if !reflect.DeepEqual(got.CueSegments, md.CueSegments) {
		t.Fatalf("segments mismatch:\n got %#v\nwant %#v", got.CueSegments, md.CueSegments)
	}

	// PCM data must survive untouched as well.
	pcm, err := NewDecoder(bytes.NewReader(out.buf)).FullPCMBuffer()
	if err != nil {
		t.Fatalf("FullPCMBuffer: %v", err)
	}
	if len(pcm.Data) != 80 || pcm.Data[79] != 79*128 {
		t.Fatalf("pcm round trip mismatch, len=%d last=%d", len(pcm.Data), pcm.Data[79])
	}
}

// TestAdtlFixtures decodes the hand built fixtures (metadata on both sides of
// the PCM chunk) and compares them against the expected model.
func TestAdtlFixtures(t *testing.T) {
	want := &Metadata{
		CuePoints: testCuePoints(),
		CueLabels: []*CueLabel{
			{CueID: idUint32(1), Text: "Intro"},
			{CueID: idUint32(2), Text: "Verse-A"},
			{CueID: idUint32(3), Text: "Coda!"},
		},
		CueNotes: []*CueNote{
			{CueID: idUint32(1), Text: "多轨注释"},
			{CueID: idUint32(2), Text: "peak"},
		},
		CueSegments: []*CueSegment{
			{
				CueID:        idUint32(3),
				SampleLength: 40,
				Purpose:      [4]byte{'r', 'g', 'n', ' '},
				CodePage:     65001,
				Text:         "fade out",
			},
		},
	}
	for _, path := range []string{"fixtures/adtl-after-data.wav", "fixtures/adtl-before-data.wav"} {
		t.Run(path, func(t *testing.T) {
			f, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			d := NewDecoder(f)
			d.ReadMetadata()
			if err := d.Err(); err != nil {
				t.Fatalf("ReadMetadata: %v", err)
			}
			got := d.Metadata
			if !reflect.DeepEqual(got.CuePoints, want.CuePoints) {
				t.Errorf("cue points:\n got %#v\nwant %#v", got.CuePoints, want.CuePoints)
			}
			if !reflect.DeepEqual(got.CueLabels, want.CueLabels) {
				t.Errorf("labels:\n got %#v\nwant %#v", got.CueLabels, want.CueLabels)
			}
			if !reflect.DeepEqual(got.CueNotes, want.CueNotes) {
				t.Errorf("notes:\n got %#v\nwant %#v", got.CueNotes, want.CueNotes)
			}
			if !reflect.DeepEqual(got.CueSegments, want.CueSegments) {
				t.Errorf("segments:\n got %#v\nwant %#v", got.CueSegments, want.CueSegments)
			}

			// the before-data fixture must still decode PCM correctly
			pcm, err := NewDecoder(mustOpen(t, path)).FullPCMBuffer()
			if err != nil {
				t.Fatalf("FullPCMBuffer: %v", err)
			}
			if len(pcm.Data) != 80 {
				t.Fatalf("expected 80 frames, got %d", len(pcm.Data))
			}
		})
	}
}

func mustOpen(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// TestEncodedBinaryLayout walks the generated container chunk by chunk and
// verifies IDs, sizes, padding bytes and the RIFF size field.
func TestEncodedBinaryLayout(t *testing.T) {
	out := encodeTestWAV(t, testMetadata())
	r := bytes.NewReader(out.buf)

	// RIFF header
	var riffID [4]byte
	r.Read(riffID[:])
	if riffID != [4]byte{'R', 'I', 'F', 'F'} {
		t.Fatalf("bad RIFF ID %v", riffID)
	}
	riffSize := readLE32(t, r)
	if int(riffSize) != len(out.buf)-8 {
		t.Fatalf("RIFF size = %d, file payload = %d", riffSize, len(out.buf)-8)
	}
	var wave [4]byte
	r.Read(wave[:])
	if wave != [4]byte{'W', 'A', 'V', 'E'} {
		t.Fatalf("bad WAVE ID %v", wave)
	}

	chunks := map[string]struct {
		size uint32
		data []byte
	}{}
	for r.Len() > 0 {
		var id [4]byte
		if _, err := r.Read(id[:]); err != nil {
			t.Fatal(err)
		}
		size := readLE32(t, r)
		data := make([]byte, size)
		if _, err := r.Read(data); err != nil {
			t.Fatalf("chunk %s: %v", id, err)
		}
		if size%2 == 1 {
			pad, err := r.ReadByte()
			if err != nil {
				t.Fatalf("missing padding after %s", id)
			}
			if pad != 0 {
				t.Errorf("padding after %s = %d, want 0", id, pad)
			}
		}
		chunks[string(id[:])] = struct {
			size uint32
			data []byte
		}{size, data}
	}

	cue, ok := chunks["cue "]
	if !ok {
		t.Fatal("cue chunk missing")
	}
	if cue.size != 4+3*24 {
		t.Errorf("cue size = %d, want %d", cue.size, 4+3*24)
	}

	if _, ok := chunks["LIST"]; !ok {
		t.Fatal("LIST chunk missing")
	}
	// locate the adtl list structurally (an INFO LIST may also be present)
	adtlPayload := extractAdtlPayload(t, out.buf)
	if adtlPayload == nil {
		t.Fatal("LIST adtl missing")
	}
	// walk the adtl sub chunks and check every declared size consumes exactly
	// the advertised bytes plus optional padding
	sr := bytes.NewReader(adtlPayload)
	counts := map[string]int{}
	for sr.Len() > 0 {
		if sr.Len() == 1 {
			sr.ReadByte()
			break
		}
		var id [4]byte
		sr.Read(id[:])
		size := readLE32(t, sr)
		payload := make([]byte, size)
		if _, err := sr.Read(payload); err != nil {
			t.Fatalf("subchunk %s truncated: %v", id, err)
		}
		if size%2 == 1 {
			pad, err := sr.ReadByte()
			if err != nil {
				t.Fatalf("missing subchunk padding after %s", id)
			}
			if pad != 0 {
				t.Errorf("subchunk %s padding = %d", id, pad)
			}
		}
		counts[string(id[:])]++
		switch string(id[:]) {
		case "labl":
			if size < 5 {
				t.Fatalf("malformed labl payload size %d", size)
			}
		case "ltxt":
			if size < ltxtFixedFields {
				t.Fatalf("ltxt too small: %d", size)
			}
		}
	}
	if counts["labl"] != 3 || counts["note"] != 2 || counts["ltxt"] != 1 {
		t.Fatalf("adtl subchunk counts = %v, want labl:3 note:2 ltxt:1", counts)
	}
}

// TestAdtlFIFOOrder ensures entries are serialized in slice order.
func TestAdtlFIFOOrder(t *testing.T) {
	out := encodeTestWAV(t, testMetadata())
	body := extractAdtlPayload(t, out.buf)
	wantOrder := []string{"labl", "labl", "labl", "note", "note", "ltxt"}
	var gotOrder []string
	for off := 0; off+8 <= len(body); {
		id := string(body[off : off+4])
		size := int(uint32(body[off+4]) | uint32(body[off+5])<<8 | uint32(body[off+6])<<16 | uint32(body[off+7])<<24)
		gotOrder = append(gotOrder, id)
		off += 8 + size + (size % 2)
	}
	if !reflect.DeepEqual(gotOrder, wantOrder) {
		t.Fatalf("subchunk order = %v, want %v", gotOrder, wantOrder)
	}
}

// TestAdtlPaddingParity builds entries with both even and odd payloads and
// verifies every sub chunk (and the wrapping LIST) lands on a word boundary
// and round trips byte identically.
func TestAdtlPaddingParity(t *testing.T) {
	md := &Metadata{
		CuePoints: []*CuePoint{
			{ID: idUint32(1), Position: 0, DataChunkID: testDataID, SampleOffset: 0},
			{ID: idUint32(2), Position: 1, DataChunkID: testDataID, SampleOffset: 1},
			{ID: idUint32(3), Position: 2, DataChunkID: testDataID, SampleOffset: 2},
			{ID: idUint32(4), Position: 3, DataChunkID: testDataID, SampleOffset: 3},
		},
		CueLabels: []*CueLabel{
			{CueID: idUint32(1), Text: "ab"},  // 4+2+1 = 7, odd
			{CueID: idUint32(2), Text: "abc"}, // 4+3+1 = 8, even
		},
		CueNotes: []*CueNote{
			{CueID: idUint32(3), Text: "x"},  // 4+1+1 = 6, even
			{CueID: idUint32(4), Text: "xy"}, // 4+2+1 = 7, odd
		},
		CueSegments: []*CueSegment{
			// no text: exactly 20 fixed bytes, even
			{CueID: idUint32(1), SampleLength: 1, Purpose: [4]byte{'r', 'g', 'n', ' '}},
			// single byte text "z" without NUL: 21 bytes, odd, needs padding
			{CueID: idUint32(2), SampleLength: 1, Purpose: [4]byte{'r', 'g', 'n', ' '}, Text: "z"},
		},
	}
	out := encodeTestWAV(t, md)
	payload := extractAdtlPayload(t, out.buf)

	// walk every sub chunk and assert the declared size plus padding consumes
	// an even number of bytes and padding bytes are zero
	r := bytes.NewReader(payload)
	for r.Len() > 0 {
		if r.Len() == 1 {
			b, _ := r.ReadByte()
			if b != 0 {
				t.Fatal("LIST level padding byte must be zero")
			}
			break
		}
		var id [4]byte
		r.Read(id[:])
		size := readLE32(t, r)
		data := make([]byte, size)
		r.Read(data)
		if size%2 == 1 {
			pad, err := r.ReadByte()
			if err != nil {
				t.Fatalf("%s: missing padding", id)
			}
			if pad != 0 {
				t.Fatalf("%s: padding = %d", id, pad)
			}
		}
	}

	// full field round trip
	d := NewDecoder(bytes.NewReader(out.buf))
	d.ReadMetadata()
	if err := d.Err(); err != nil {
		t.Fatal(err)
	}
	got := d.Metadata
	if !reflect.DeepEqual(got.CueLabels, md.CueLabels) ||
		!reflect.DeepEqual(got.CueNotes, md.CueNotes) ||
		!reflect.DeepEqual(got.CueSegments, md.CueSegments) {
		t.Fatalf("parity round trip mismatch:\nlabels %#v\nnotes %#v\nsegs %#v",
			got.CueLabels, got.CueNotes, got.CueSegments)
	}
}

// TestPCMBeforeMetadata ensures metadata chunks placed before the PCM data
// survive a FullPCMBuffer -> ReadMetadata sequence (they must be decoded while
// forwarding to the PCM chunk rather than drained).
func TestPCMBeforeMetadata(t *testing.T) {
	for _, path := range []string{"fixtures/adtl-before-data.wav", "fixtures/adtl-after-data.wav"} {
		t.Run(path, func(t *testing.T) {
			f := mustOpen(t, path)
			defer f.Close()
			d := NewDecoder(f)
			pcm, err := d.FullPCMBuffer()
			if err != nil {
				t.Fatalf("FullPCMBuffer: %v", err)
			}
			if len(pcm.Data) != 80 {
				t.Fatalf("expected 80 frames, got %d", len(pcm.Data))
			}
			d.ReadMetadata()
			if err := d.Err(); err != nil {
				t.Fatalf("ReadMetadata after PCM: %v", err)
			}
			md := d.Metadata
			if len(md.CuePoints) != 3 || len(md.CueLabels) != 3 ||
				len(md.CueNotes) != 2 || len(md.CueSegments) != 1 {
				t.Fatalf("metadata lost after PCM read: cues=%d labels=%d notes=%d segs=%d",
					len(md.CuePoints), len(md.CueLabels), len(md.CueNotes), len(md.CueSegments))
			}
			if md.CueLabels[0].Text != "Intro" || md.CueNotes[0].Text != "多轨注释" {
				t.Fatalf("unexpected text content: %#v %#v", md.CueLabels[0], md.CueNotes[0])
			}
		})
	}
}

// TestDecodeErrors covers the explicit error conditions.
func TestDecodeErrors(t *testing.T) {
	good := encodeTestWAV(t, testMetadata()).buf

	t.Run("unknown cue ID", func(t *testing.T) {
		// flip the cue ID inside the first labl subchunk to 99
		bad := mutateAdtlCueID(t, good, "labl", idUint32(99))
		d := NewDecoder(bytes.NewReader(bad))
		d.ReadMetadata()
		if !errors.Is(d.Err(), ErrUnknownCueID) {
			t.Fatalf("err = %v, want ErrUnknownCueID", d.Err())
		}
	})

	t.Run("duplicate labl", func(t *testing.T) {
		bad := bytes.Clone(good)
		// duplicate the first labl sub chunk (8 header + 10 payload bytes)
		// inside the adtl LIST, keeping every size field consistent
		listPos := locateAdtlList(t, bad)
		listSize := binary.LittleEndian.Uint32(bad[listPos+4:])
		subStart := listPos + 8 + 4 // skip LIST header and the "adtl" marker
		subLen := 8 + 10
		dup := bytes.Clone(bad[subStart : subStart+subLen])
		insertAt := subStart + subLen
		bad = append(bad[:insertAt], append(dup, bad[insertAt:]...)...)
		binary.LittleEndian.PutUint32(bad[listPos+4:], listSize+uint32(subLen))
		binary.LittleEndian.PutUint32(bad[4:], uint32(len(bad))-8)

		d := NewDecoder(bytes.NewReader(bad))
		d.ReadMetadata()
		if !errors.Is(d.Err(), ErrDuplicateAdtlEntry) {
			t.Fatalf("err = %v, want ErrDuplicateAdtlEntry", d.Err())
		}
	})

	t.Run("truncated sub chunk", func(t *testing.T) {
		bad := bytes.Clone(good)
		// claim a huge size on the first labl
		start := bytes.Index(bad, markerLabl[:]) + 4
		bad[start] = 0xFF
		bad[start+1] = 0xFF
		d := NewDecoder(bytes.NewReader(bad))
		d.ReadMetadata()
		if !errors.Is(d.Err(), ErrTruncatedChunk) {
			t.Fatalf("err = %v, want ErrTruncatedChunk", d.Err())
		}
	})

	t.Run("truncated ltxt fixed fields", func(t *testing.T) {
		bad := bytes.Clone(good)
		start := bytes.Index(bad, markerLtxt[:]) + 4
		// shrink the ltxt declared size below the 20 fixed bytes
		bad[start] = 4
		bad[start+1] = 0
		bad[start+2] = 0
		bad[start+3] = 0
		d := NewDecoder(bytes.NewReader(bad))
		d.ReadMetadata()
		if !errors.Is(d.Err(), ErrTruncatedChunk) {
			t.Fatalf("err = %v, want ErrTruncatedChunk", d.Err())
		}
	})

	t.Run("segment past audio length", func(t *testing.T) {
		md := testMetadata()
		md.CueSegments[0].SampleLength = 41 // frame 40 + 41 > 80
		bad := encodeTestWAV(t, md).buf
		d := NewDecoder(bytes.NewReader(bad))
		d.ReadMetadata()
		if !errors.Is(d.Err(), ErrSegmentOutOfRange) {
			t.Fatalf("decoder err = %v, want ErrSegmentOutOfRange", d.Err())
		}
	})
}

// TestEncodeErrors covers validation failures caught while writing.
func TestEncodeErrors(t *testing.T) {
	t.Run("unknown cue ID", func(t *testing.T) {
		md := testMetadata()
		md.CueLabels[0].CueID = idUint32(99)
		out := &memFile{}
		e := NewEncoder(out, 8000, 16, 1, 1)
		e.Metadata = md
		if err := e.Write(silentBuffer()); err != nil {
			t.Fatal(err)
		}
		if err := e.Close(); !errors.Is(err, ErrUnknownCueID) {
			t.Fatalf("err = %v, want ErrUnknownCueID", err)
		}
	})

	t.Run("duplicate entry", func(t *testing.T) {
		md := testMetadata()
		md.CueLabels = append(md.CueLabels, &CueLabel{CueID: idUint32(1), Text: "dup"})
		out := &memFile{}
		e := NewEncoder(out, 8000, 16, 1, 1)
		e.Metadata = md
		if err := e.Write(silentBuffer()); err != nil {
			t.Fatal(err)
		}
		if err := e.Close(); !errors.Is(err, ErrDuplicateAdtlEntry) {
			t.Fatalf("err = %v, want ErrDuplicateAdtlEntry", err)
		}
	})

	t.Run("invalid UTF-8", func(t *testing.T) {
		md := testMetadata()
		md.CueLabels[0].Text = "bad\xff\xffutf8"
		out := &memFile{}
		e := NewEncoder(out, 8000, 16, 1, 1)
		e.Metadata = md
		if err := e.Write(silentBuffer()); err != nil {
			t.Fatal(err)
		}
		if err := e.Close(); !errors.Is(err, ErrInvalidUTF8) {
			t.Fatalf("err = %v, want ErrInvalidUTF8", err)
		}
	})

	t.Run("text too long", func(t *testing.T) {
		if err := checkText(string(make([]byte, maxChunkSize)), true); !errors.Is(err, ErrTextTooLong) {
			t.Fatalf("err = %v, want ErrTextTooLong", err)
		}
	})
}

// TestRepeatedCloseDoesNotOverwrite writes audio in several calls, then closes
// twice; the metadata must appear exactly once and stay readable.
func TestRepeatedCloseDoesNotOverwrite(t *testing.T) {
	out := &memFile{}
	e := NewEncoder(out, 8000, 16, 1, 1)
	half := &audio.IntBuffer{
		Format: &audio.Format{NumChannels: 1, SampleRate: 8000},
		Data:   make([]int, 40),
	}
	if err := e.Write(half); err != nil {
		t.Fatal(err)
	}
	if err := e.Write(half); err != nil {
		t.Fatal(err)
	}
	e.Metadata = testMetadata()
	if err := e.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}

	// count chunks structurally so patterns inside the PCM payload can't
	// produce false positives. One INFO LIST (the Title field) and one adtl
	// LIST are expected, each exactly once.
	chunkCount := countTopChunks(t, out.buf)
	if n := chunkCount["LIST"]; n != 2 {
		t.Fatalf("LIST chunks found %d times, want 2 (INFO + adtl)", n)
	}
	if n := chunkCount["cue "]; n != 1 {
		t.Fatalf("cue chunk found %d times, want 1", n)
	}
	if payload := extractAdtlPayload(t, out.buf); len(payload) == 0 {
		t.Fatal("adtl LIST is empty after a repeated Close")
	}
	d := NewDecoder(bytes.NewReader(out.buf))
	d.ReadMetadata()
	if err := d.Err(); err != nil {
		t.Fatalf("re-read after double close: %v", err)
	}
	if !reflect.DeepEqual(d.Metadata.CueLabels, e.Metadata.CueLabels) {
		t.Fatal("labels lost after repeated Close")
	}
	pcm, err := NewDecoder(bytes.NewReader(out.buf)).FullPCMBuffer()
	if err != nil || len(pcm.Data) != 80 {
		t.Fatalf("pcm after double close: len=%d err=%v", len(pcm.Data), err)
	}
}

// TestOddPCMPadding writes an odd sized data chunk and checks the padding byte
// plus chunk size handling: the declared data size stays odd, the physical
// chunk occupies an even number of bytes and the RIFF size matches the file.
func TestOddPCMPadding(t *testing.T) {
	out := &memFile{}
	// 8 bit mono, 3 frames -> 3 PCM bytes (odd)
	e := NewEncoder(out, 8000, 8, 1, 1)
	buf := &audio.IntBuffer{
		Format: &audio.Format{NumChannels: 1, SampleRate: 8000},
		Data:   []int{1, 2, 3},
	}
	if err := e.Write(buf); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	raw := out.buf
	if got := binary.LittleEndian.Uint32(raw[4:8]); int(got) != len(raw)-8 {
		t.Fatalf("RIFF size = %d, want %d", got, len(raw)-8)
	}
	dataIdx := bytes.Index(raw, []byte{'d', 'a', 't', 'a'})
	if dataIdx < 0 {
		t.Fatal("data chunk not found")
	}
	dataSize := binary.LittleEndian.Uint32(raw[dataIdx+4:])
	if dataSize != 3 {
		t.Fatalf("declared data size = %d, want 3 (padding excluded)", dataSize)
	}
	if raw[dataIdx+8+3] != 0 {
		t.Fatalf("padding byte = %d, want 0", raw[dataIdx+8+3])
	}
	// the next chunk header (if any) must start on an even boundary: total
	// header(12) + fmt(24) + data header(8) + 3 bytes + 1 pad = 48
	if (dataIdx+8+int(dataSize)+1)%2 != 0 {
		t.Fatal("padding did not restore word alignment")
	}
}

// helpers --------------------------------------------------------------------

func silentBuffer() *audio.IntBuffer {
	return &audio.IntBuffer{
		Format: &audio.Format{NumChannels: 1, SampleRate: 8000},
		Data:   make([]int, 80),
	}
}

// locateAdtlList walks the RIFF container and returns the absolute offset of
// the LIST "adtl" chunk header.
func locateAdtlList(t *testing.T, raw []byte) int {
	t.Helper()
	off := 12 // skip RIFF header + WAVE marker
	for off+8 <= len(raw) {
		cid := string(raw[off : off+4])
		size := int(binary.LittleEndian.Uint32(raw[off+4:]))
		if cid == "LIST" && bytes.Equal(raw[off+8:off+12], markerAdtl[:]) {
			return off
		}
		off += 8 + size + (size % 2)
	}
	t.Fatal("LIST adtl chunk not found")
	return -1
}

// extractAdtlPayload locates the LIST "adtl" chunk structurally and returns
// its sub chunk bytes (everything after the "adtl" marker).
func extractAdtlPayload(t *testing.T, raw []byte) []byte {
	t.Helper()
	r := bytes.NewReader(raw[12:]) // skip RIFF header + WAVE
	for r.Len() > 0 {
		var id [4]byte
		if _, err := r.Read(id[:]); err != nil {
			t.Fatal(err)
		}
		b := make([]byte, 4)
		if _, err := r.Read(b); err != nil {
			t.Fatal(err)
		}
		size := int(binary.LittleEndian.Uint32(b))
		data := make([]byte, size)
		if _, err := r.Read(data); err != nil {
			t.Fatal(err)
		}
		if size%2 == 1 {
			r.Seek(1, io.SeekCurrent)
		}
		if id == CIDList && bytes.HasPrefix(data, markerAdtl[:]) {
			return data[4:]
		}
	}
	t.Fatal("LIST adtl chunk not found")
	return nil
}

// countTopChunks walks the RIFF container structurally and counts its sub
// chunks by ID, honoring the word alignment padding.
func countTopChunks(t *testing.T, raw []byte) map[string]int {
	t.Helper()
	r := bytes.NewReader(raw[12:]) // skip RIFF header + WAVE
	counts := map[string]int{}
	for r.Len() > 0 {
		var id [4]byte
		if _, err := r.Read(id[:]); err != nil {
			t.Fatal(err)
		}
		b := make([]byte, 4)
		if _, err := r.Read(b); err != nil {
			t.Fatal(err)
		}
		size := int(binary.LittleEndian.Uint32(b))
		if _, err := r.Seek(int64(size), io.SeekCurrent); err != nil {
			t.Fatal(err)
		}
		if size%2 == 1 {
			r.Seek(1, io.SeekCurrent)
		}
		counts[string(id[:])]++
	}
	return counts
}

func readLE32(t *testing.T, r interface{ Read([]byte) (int, error) }) uint32 {
	t.Helper()
	b := make([]byte, 4)
	if _, err := r.Read(b); err != nil {
		t.Fatal(err)
	}
	return binary.LittleEndian.Uint32(b)
}

func mutateAdtlCueID(t *testing.T, wav []byte, marker string, newID [4]byte) []byte {
	t.Helper()
	var m [4]byte
	switch marker {
	case "labl":
		m = markerLabl
	case "note":
		m = markerNote
	case "ltxt":
		m = markerLtxt
	}
	idx := bytes.Index(wav, m[:])
	if idx < 0 {
		t.Fatalf("marker %s not found", marker)
	}
	out := bytes.Clone(wav)
	copy(out[idx+8:idx+12], newID[:])
	return out
}
