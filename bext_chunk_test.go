package wav

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/go-audio/audio"
	"github.com/go-audio/riff"
)

// memWriteSeeker is an in memory io.WriteSeeker used to inspect the exact
// bytes produced by the Encoder.
type memWriteSeeker struct {
	buf bytes.Buffer
	pos int64
}

func (m *memWriteSeeker) Write(p []byte) (int, error) {
	// bytes.Buffer doesn't support seeking, emulate random access writes.
	end := int64(len(p)) + m.pos
	if end > int64(m.buf.Len()) {
		grow := make([]byte, end-int64(m.buf.Len()))
		if _, err := m.buf.Write(grow); err != nil {
			return 0, err
		}
	}
	raw := m.buf.Bytes()
	n := copy(raw[m.pos:], p)
	m.pos += int64(n)
	return n, nil
}

func (m *memWriteSeeker) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		m.pos = offset
	case io.SeekCurrent:
		m.pos += offset
	case io.SeekEnd:
		m.pos = int64(m.buf.Len()) + offset
	}
	if m.pos < 0 {
		return 0, errors.New("negative position")
	}
	return m.pos, nil
}

func sampleBext(history string) *Bext {
	return &Bext{
		Description:         "field recording of the morning birds",
		Originator:          "Radio Nature Unit",
		OriginatorReference: "RNU-2026-1116-0001",
		OriginationDate:     "2026-11-16",
		OriginationTime:     "09:30:00",
		TimeReference:       1 << 40,
		CodingHistory:       history,
	}
}

func encodeInMemory(t *testing.T, b *Bext, metadata *Metadata, frames int) []byte {
	t.Helper()
	out := &memWriteSeeker{}
	e := NewEncoder(out, 48000, 16, 1, 1)
	// split the PCM data across multiple Write calls on purpose
	half := frames / 2
	first := &audio.IntBuffer{
		Format: &audio.Format{NumChannels: 1, SampleRate: 48000},
		Data:   make([]int, half),
	}
	second := &audio.IntBuffer{
		Format: &audio.Format{NumChannels: 1, SampleRate: 48000},
		Data:   make([]int, frames-half),
	}
	for i := range first.Data {
		first.Data[i] = int(int16(i + 1))
	}
	for i := range second.Data {
		second.Data[i] = int(int16(-(i + 1)))
	}
	if err := e.Write(first); err != nil {
		t.Fatalf("first Write: %v", err)
	}
	if err := e.Write(second); err != nil {
		t.Fatalf("second Write: %v", err)
	}
	e.Bext = b
	e.Metadata = metadata
	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return out.buf.Bytes()
}

// walkChunks parses every top level RIFF chunk of a WAVE file and reports the
// sequence of (ID, declared size, payload), including odd byte padding.
func walkChunks(t *testing.T, raw []byte) []struct {
	id      [4]byte
	size    uint32
	payload []byte
} {
	t.Helper()
	if !bytes.Equal(raw[0:4], riff.RiffID[:]) {
		t.Fatalf("missing RIFF header")
	}
	riffSize := binary.LittleEndian.Uint32(raw[4:8])
	if int(riffSize)+8 != len(raw) {
		t.Fatalf("RIFF size field: got %d, expected %d", riffSize, len(raw)-8)
	}
	if !bytes.Equal(raw[8:12], riff.WavFormatID[:]) {
		t.Fatalf("missing WAVE ID")
	}
	var out []struct {
		id      [4]byte
		size    uint32
		payload []byte
	}
	pos := 12
	for pos+8 <= len(raw) {
		var id [4]byte
		copy(id[:], raw[pos:pos+4])
		size := binary.LittleEndian.Uint32(raw[pos+4 : pos+8])
		start := pos + 8
		end := start + int(size)
		if end > len(raw) {
			t.Fatalf("chunk %q of size %d runs past end of file (pos %d, file %d)", id, size, pos, len(raw))
		}
		out = append(out, struct {
			id      [4]byte
			size    uint32
			payload []byte
		}{id, size, raw[start:end]})
		pos = end
		if size%2 == 1 {
			if pos >= len(raw) || raw[pos] != 0 {
				t.Fatalf("chunk %q missing its zero padding byte", id)
			}
			pos++
		}
	}
	if pos != len(raw) {
		t.Fatalf("%d trailing bytes after the last chunk", len(raw)-pos)
	}
	return out
}

func TestBext_RoundTripAndLayout(t *testing.T) {
	// CodingHistory of both parities exercises the odd byte padding path.
	histories := []struct {
		name string
		text string
	}{
		{"even", "A=PCM,F=48000,W=16,M=mono,T=go-audio\r\n"},
		{"odd", "A=PCM,F=48000,W=16,M=mono,T=go-audio\r\nX"},
		{"empty", ""},
	}
	for _, hc := range histories {
		t.Run(hc.name, func(t *testing.T) {
			want := sampleBext(hc.text)
			wantMetadata := &Metadata{Artist: "The Birds", Title: "Dawn chorus"}
			frames := 7 // odd number of 16 bit mono frames => odd data chunk too
			raw := encodeInMemory(t, want, wantMetadata, frames)

			chunks := walkChunks(t, raw)

			// expected ordering: fmt, data, bext, LIST
			var ids [][4]byte
			var dataChunk, bextChunk, listChunk int
			for i, c := range chunks {
				ids = append(ids, c.id)
				switch c.id {
				case riff.DataFormatID:
					dataChunk = i
				case CIDBext:
					bextChunk = i
				case CIDList:
					listChunk = i
				}
			}
			if dataChunk == 0 || bextChunk != dataChunk+1 || listChunk != bextChunk+1 {
				t.Fatalf("unexpected chunk order: %v", ids)
			}

			// data chunk still holds all the samples, nothing got overwritten
			if got := chunks[dataChunk].size; got != uint32(frames*2) {
				t.Fatalf("data chunk size = %d, expected %d", got, frames*2)
			}

			bc := chunks[bextChunk]
			expectedSize := uint32(bextV0FixedSize + len(hc.text))
			if bc.size != expectedSize {
				t.Fatalf("bext declared size = %d, expected %d", bc.size, expectedSize)
			}
			// an odd payload must be followed by one zero padding byte; the
			// RIFF size covers all content, walkChunks already asserted the
			// padding byte position and value.
			if len(raw)%2 != 0 {
				t.Fatalf("file must stay word aligned, length = %d", len(raw))
			}
			if len(bc.payload) != int(expectedSize) {
				t.Fatalf("bext payload length = %d, expected %d", len(bc.payload), expectedSize)
			}

			// decode the written file back: PCM must be read before
			// ReadMetadata which walks the file to its end
			d := NewDecoder(bytes.NewReader(raw))
			pcm, err := d.FullPCMBuffer()
			if err != nil {
				t.Fatalf("FullPCMBuffer: %v", err)
			}
			d.ReadMetadata()
			if err := d.Err(); err != nil {
				t.Fatalf("ReadMetadata: %v", err)
			}
			if d.Bext == nil {
				t.Fatal("decoder did not expose a bext chunk")
			}
			got := d.Bext
			if got.Version != 0 {
				t.Errorf("Version = %d, expected 0", got.Version)
			}
			if got.Description != want.Description ||
				got.Originator != want.Originator ||
				got.OriginatorReference != want.OriginatorReference ||
				got.OriginationDate != want.OriginationDate ||
				got.OriginationTime != want.OriginationTime ||
				got.TimeReference != want.TimeReference ||
				got.CodingHistory != want.CodingHistory {
				t.Fatalf("bext round trip mismatch\nwant %#v\ngot  %#v", want, got)
			}
			if d.Metadata == nil || d.Metadata.Artist != "The Birds" || d.Metadata.Title != "Dawn chorus" {
				t.Fatalf("LIST metadata not preserved: %#v", d.Metadata)
			}

			// PCM samples untouched
			if len(pcm.Data) != frames {
				t.Fatalf("frame count = %d, expected %d", len(pcm.Data), frames)
			}
			expectedSamples := make([]int, frames)
			half := frames / 2
			for i := 0; i < half; i++ {
				expectedSamples[i] = i + 1
			}
			for i := half; i < frames; i++ {
				expectedSamples[i] = -(i - half + 1)
			}
			if !reflect.DeepEqual(pcm.Data, expectedSamples) {
				t.Fatalf("PCM mismatch\nwant %v\ngot  %v", expectedSamples, pcm.Data)
			}
		})
	}
}

func TestBext_RawFixedFieldLayout(t *testing.T) {
	b := &Bext{
		Description:         "desc",
		Originator:          "org",
		OriginatorReference: "ref",
		OriginationDate:     "2026-11-16",
		OriginationTime:     "09:30:00",
		TimeReference:       0x0102030405060708,
	}
	raw, err := encodeBextChunk(b)
	if err != nil {
		t.Fatalf("encodeBextChunk: %v", err)
	}
	if len(raw) != bextV0FixedSize {
		t.Fatalf("encoded size = %d, expected %d", len(raw), bextV0FixedSize)
	}
	checks := []struct {
		name string
		off  int
		size int
		want string
	}{
		{"Description", 0, bextDescriptionSize, "desc"},
		{"Originator", bextDescriptionSize, bextOriginatorSize, "org"},
		{"OriginatorReference", bextDescriptionSize + bextOriginatorSize, bextOriginatorReferenceSize, "ref"},
		{"OriginationDate", bextDescriptionSize + bextOriginatorSize + bextOriginatorReferenceSize, bextOriginationDateSize, "2026-11-16"},
		{"OriginationTime", bextDescriptionSize + bextOriginatorSize + bextOriginatorReferenceSize + bextOriginationDateSize, bextOriginationTimeSize, "09:30:00"},
	}
	for _, c := range checks {
		slot := raw[c.off : c.off+c.size]
		if got := nullTermStr(slot); got != c.want {
			t.Errorf("%s = %q, expected %q", c.name, got, c.want)
		}
		// NUL terminator right after the content when the slot has room;
		// the fixed width date/time fields may fill their whole slot
		if len(c.want) < c.size && slot[len(c.want)] != 0 {
			t.Errorf("%s missing NUL terminator", c.name)
		}
	}
	trOff := bextDescriptionSize + bextOriginatorSize + bextOriginatorReferenceSize +
		bextOriginationDateSize + bextOriginationTimeSize
	if got := binary.LittleEndian.Uint64(raw[trOff:]); got != b.TimeReference {
		t.Errorf("TimeReference = %#x, expected %#x", got, b.TimeReference)
	}
	if got := binary.LittleEndian.Uint16(raw[trOff+bextTimeReferenceSize:]); got != 0 {
		t.Errorf("Version = %d, expected 0", got)
	}
}

func TestBext_DecodeBWFFixture(t *testing.T) {
	f, err := os.Open("fixtures/bwf.wav")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	d := NewDecoder(f)
	d.ReadInfo()
	if err := d.Err(); err != nil {
		t.Fatalf("ReadInfo: %v", err)
	}
	if d.Bext == nil {
		t.Fatal("expected bext metadata in fixtures/bwf.wav")
	}
	b := d.Bext
	// values verified directly from the fixture bytes
	want := &Bext{
		Description:         "",
		Originator:          "Logic Pro",
		OriginatorReference: "",          // the reference slot starts with NULs in this fixture
		OriginationDate:     "2011-05-0", // exactly what the file stores
		OriginationTime:     "14:48:2",
		TimeReference:       0,
		Version:             1,
	}
	if b.Description != want.Description ||
		b.Originator != want.Originator ||
		b.OriginatorReference != want.OriginatorReference ||
		b.OriginationDate != want.OriginationDate ||
		b.OriginationTime != want.OriginationTime ||
		b.TimeReference != want.TimeReference ||
		b.Version != want.Version {
		t.Fatalf("bwf.wav bext mismatch\nwant %#v\ngot  %#v", want, b)
	}
	// the file must still decode as valid audio
	if !d.IsValidFile() {
		t.Fatal("bwf.wav no longer considered valid")
	}
}

func TestBext_TruncatedChunk(t *testing.T) {
	// craft a RIFF/WAVE container whose bext chunk ends mid fixed fields
	var buf bytes.Buffer
	buf.Write(riff.RiffID[:])
	binary.Write(&buf, binary.LittleEndian, uint32(0)) // placeholder
	buf.Write(riff.WavFormatID[:])
	buf.Write(CIDBext[:])
	binary.Write(&buf, binary.LittleEndian, uint32(42)) // smaller than bextV0FixedSize
	buf.Write(make([]byte, 42))

	d := NewDecoder(bytes.NewReader(buf.Bytes()))
	d.ReadMetadata()
	if !errors.Is(d.Err(), ErrBextTruncated) {
		t.Fatalf("expected ErrBextTruncated, got %v", d.Err())
	}

	// direct decoder API on a full sized header but short payload
	ch := &riff.Chunk{
		ID:   CIDBext,
		Size: bextV0FixedSize - 1,
		R:    io.LimitReader(bytes.NewReader(make([]byte, bextV0FixedSize)), int64(bextV0FixedSize-1)),
	}
	d2 := NewDecoder(bytes.NewReader(nil))
	if err := DecodeBextChunk(d2, ch); !errors.Is(err, ErrBextTruncated) {
		t.Fatalf("expected ErrBextTruncated from direct decode, got %v", err)
	}
}

// buildVersionedBext crafts a raw bext payload for BWF version 1 or 2,
// filling the UMID and (for v2) loudness extension with recognizable bytes so
// the CodingHistory offset can be validated independently.
func buildVersionedBext(t *testing.T, version uint16, history string) []byte {
	t.Helper()
	payload := make([]byte, bextV0FixedSize)
	// description lives in the first slot
	copy(payload[0:], "synthetic")
	binary.LittleEndian.PutUint64(payload[bextDescriptionSize+bextOriginatorSize+bextOriginatorReferenceSize+
		bextOriginationDateSize+bextOriginationTimeSize:], 0x1122334455667788)
	binary.LittleEndian.PutUint16(payload[bextV0FixedSize-bextVersionSize:], version)

	// version 1: 64 byte UMID, mark its boundaries
	umid := make([]byte, bextUMIDSize)
	umid[0], umid[bextUMIDSize-1] = 0xAA, 0xBB
	payload = append(payload, umid...)
	if version >= 2 {
		// five int16 loudness fields
		loud := make([]byte, bextV2ExtraSize)
		for i := range loud {
			loud[i] = 0xCC
		}
		payload = append(payload, loud...)
	}
	payload = append(payload, []byte(history)...)
	return payload
}

func TestBext_VersionedChunks(t *testing.T) {
	history := "A=PCM,F=48000\r\n"
	for _, version := range []uint16{1, 2} {
		t.Run(fmt.Sprintf("version%d", version), func(t *testing.T) {
			raw := buildVersionedBext(t, version, history)
			ch := &riff.Chunk{
				ID:   CIDBext,
				Size: len(raw),
				R:    bytes.NewReader(raw),
			}
			d := NewDecoder(bytes.NewReader(nil))
			if err := DecodeBextChunk(d, ch); err != nil {
				t.Fatalf("version %d decode: %v", version, err)
			}
			if d.Bext == nil {
				t.Fatal("no bext exposed")
			}
			if d.Bext.Version != version {
				t.Errorf("Version = %d, expected %d", d.Bext.Version, version)
			}
			if d.Bext.Description != "synthetic" {
				t.Errorf("Description = %q", d.Bext.Description)
			}
			if d.Bext.TimeReference != 0x1122334455667788 {
				t.Errorf("TimeReference = %#x", d.Bext.TimeReference)
			}
			if d.Bext.CodingHistory != history {
				t.Errorf("CodingHistory = %q, expected %q", d.Bext.CodingHistory, history)
			}
			// the whole payload must have been consumed
			if ch.Pos != ch.Size {
				t.Errorf("chunk not fully read: Pos=%d Size=%d", ch.Pos, ch.Size)
			}
		})
	}

	// version 1 chunk claiming an extension that does not fit must error
	truncated := buildVersionedBext(t, 1, "")
	ch := &riff.Chunk{
		ID:   CIDBext,
		Size: bextV0FixedSize + 10, // claims 64 byte UMID but only provides 10
		R:    bytes.NewReader(truncated[:bextV0FixedSize+10]),
	}
	d := NewDecoder(bytes.NewReader(nil))
	if err := DecodeBextChunk(d, ch); !errors.Is(err, ErrBextTruncated) {
		t.Fatalf("expected ErrBextTruncated for undersized v1 chunk, got %v", err)
	}
}

func TestBext_ValidationErrors(t *testing.T) {
	long256 := string(make([]byte, bextDescriptionSize)) // needs a NUL => 257 > 256
	long33 := string(make([]byte, bextOriginatorSize+1))
	testCases := []struct {
		name string
		bext *Bext
		want error
	}{
		{"description too long", &Bext{Description: long256}, ErrBextFieldTooLong},
		{"originator too long", &Bext{Originator: long33}, ErrBextFieldTooLong},
		{"reference too long", &Bext{OriginatorReference: long33}, ErrBextFieldTooLong},
		{"date 11 chars", &Bext{OriginationDate: "2026-11-160"}, ErrBextFieldTooLong},
		{"time 9 chars", &Bext{OriginationTime: "09:30:000"}, ErrBextFieldTooLong},
		{"bad date format", &Bext{OriginationDate: "16/11/2026"}, ErrBextInvalidDate},
		{"impossible date", &Bext{OriginationDate: "2026-13-40"}, ErrBextInvalidDate},
		{"bad time format", &Bext{OriginationTime: "9:30"}, ErrBextInvalidTime},
		{"impossible time", &Bext{OriginationTime: "25:61:61"}, ErrBextInvalidTime},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := encodeBextChunk(tc.bext); !errors.Is(err, tc.want) {
				t.Fatalf("expected %v, got %v", tc.want, err)
			}
			// the encoder Close path must surface the same error instead of
			// silently dropping source information
			out := &memWriteSeeker{}
			e := NewEncoder(out, 8000, 16, 1, 1)
			e.Bext = tc.bext
			if err := e.Close(); !errors.Is(err, tc.want) {
				t.Fatalf("Close expected %v, got %v", tc.want, err)
			}
		})
	}
}

func TestBext_TimeReferenceConversion(t *testing.T) {
	// 1.5 seconds at 48kHz => 72000 samples
	ref, err := TimeReferenceFromDuration(1500*time.Millisecond, 48000)
	if err != nil {
		t.Fatal(err)
	}
	if ref != 72000 {
		t.Fatalf("ref = %d, expected 72000", ref)
	}
	if _, err := TimeReferenceFromDuration(-time.Second, 48000); !errors.Is(err, ErrBextTimeReferenceOutOfRange) {
		t.Fatalf("negative duration: expected range error, got %v", err)
	}
	if _, err := TimeReferenceFromDuration(time.Second, 0); !errors.Is(err, ErrBextTimeReferenceOutOfRange) {
		t.Fatalf("zero sample rate: expected range error, got %v", err)
	}
	// a sample count that cannot fit in the unsigned 64 bit TimeReference
	hugeRate := uint64(1 << 63)
	if _, err := TimeReferenceFromDuration(2*time.Second, hugeRate); !errors.Is(err, ErrBextTimeReferenceOutOfRange) {
		t.Fatalf("overflow: expected range error, got %v", err)
	}

	b := &Bext{}
	if err := b.SetTimeReference(time.Second, 44100); err != nil {
		t.Fatal(err)
	}
	if b.TimeReference != 44100 {
		t.Fatalf("SetTimeReference = %d, expected 44100", b.TimeReference)
	}
}

func TestBext_NoBextKeepsPlainWAV(t *testing.T) {
	out := &memWriteSeeker{}
	e := NewEncoder(out, 22050, 16, 1, 1)
	buf := &audio.IntBuffer{
		Format: &audio.Format{NumChannels: 1, SampleRate: 22050},
		Data:   []int{1, 2, 3, 4},
	}
	if err := e.Write(buf); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	raw := out.buf.Bytes()
	if bytes.Contains(raw, CIDBext[:]) {
		t.Fatal("a bext chunk was written despite Bext being nil")
	}
	chunks := walkChunks(t, raw)
	for _, c := range chunks {
		if c.id == CIDBext {
			t.Fatal("unexpected bext chunk")
		}
	}
	d := NewDecoder(bytes.NewReader(raw))
	if !d.IsValidFile() {
		t.Fatal("plain WAV without bext is no longer valid")
	}
	d.ReadMetadata()
	if err := d.Err(); err != nil {
		t.Fatal(err)
	}
	if d.Bext != nil {
		t.Fatalf("Bext should stay nil, got %#v", d.Bext)
	}
}

func TestBext_CueAndListChunksNotOverwritten(t *testing.T) {
	// flloop.wav contains cue + smpl chunks; listinfo.wav contains a LIST chunk.
	// Re-encoding with bext must keep the audio intact and append bext without
	// disturbing the other trailing chunks' layout rules.
	for _, in := range []string{"fixtures/flloop.wav", "fixtures/listinfo.wav"} {
		t.Run(in, func(t *testing.T) {
			f, err := os.Open(in)
			if err != nil {
				t.Fatal(err)
			}
			d := NewDecoder(f)
			pcm, err := d.FullPCMBuffer()
			if err != nil {
				t.Fatalf("FullPCMBuffer: %v", err)
			}
			f.Close()

			out := &memWriteSeeker{}
			e := NewEncoder(out, int(d.SampleRate), int(d.BitDepth), int(d.NumChans), int(d.WavAudioFormat))
			if err := e.Write(pcm); err != nil {
				t.Fatal(err)
			}
			e.Bext = sampleBext("A=PCM\r\n")
			if err := e.Close(); err != nil {
				t.Fatal(err)
			}
			chunks := walkChunks(t, out.buf.Bytes())
			sawBext, sawData := false, false
			for _, c := range chunks {
				if c.id == CIDBext {
					sawBext = true
					if c.size != uint32(bextV0FixedSize+len("A=PCM\r\n")) {
						t.Fatalf("bext size = %d", c.size)
					}
				}
				if c.id == riff.DataFormatID {
					sawData = true
					if int(c.size) != len(pcm.Data)*(int(d.BitDepth)/8) {
						t.Fatalf("data chunk size changed: %d", c.size)
					}
				}
			}
			if !sawBext || !sawData {
				t.Fatalf("missing chunks, saw bext=%v data=%v", sawBext, sawData)
			}

			nd := NewDecoder(bytes.NewReader(out.buf.Bytes()))
			rt, err := nd.FullPCMBuffer()
			if err != nil {
				t.Fatalf("round trip PCM: %v", err)
			}
			if len(rt.Data) != len(pcm.Data) {
				t.Fatalf("sample count changed: %d vs %d", len(rt.Data), len(pcm.Data))
			}
			for i := range pcm.Data {
				if pcm.Data[i] != rt.Data[i] {
					t.Fatalf("sample %d changed: %d -> %d", i, pcm.Data[i], rt.Data[i])
				}
			}
		})
	}
}
