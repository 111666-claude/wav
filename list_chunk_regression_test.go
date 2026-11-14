package wav

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-audio/audio"
	"github.com/go-audio/riff"
)

// infoSubchunk serializes one INFO subchunk entry. When pad is true, a NUL
// word-alignment byte is appended after odd-length values (canonical RIFF);
// when false, entries are packed back to back (as this package's encoder and
// many other writers produce).
func infoSubchunk(id string, val []byte, pad bool) []byte {
	var b bytes.Buffer
	b.WriteString(id)
	binary.Write(&b, binary.LittleEndian, uint32(len(val)))
	b.Write(val)
	if pad && len(val)%2 == 1 {
		b.WriteByte(0x00)
	}
	return b.Bytes()
}

// wavWithList wraps a LIST body in a minimal valid WAV container. The LIST
// chunk is placed after the fmt chunk and before the data chunk. If the LIST
// size is odd, the RIFF word-alignment byte is appended after the body.
func wavWithList(t *testing.T, listBody []byte) []byte {
	t.Helper()
	var wav bytes.Buffer
	wav.WriteString("RIFF")
	binary.Write(&wav, binary.LittleEndian, uint32(0)) // patched below
	wav.WriteString("WAVE")
	wav.WriteString("fmt ")
	binary.Write(&wav, binary.LittleEndian, uint32(16))
	binary.Write(&wav, binary.LittleEndian, uint16(1))     // PCM
	binary.Write(&wav, binary.LittleEndian, uint16(1))     // mono
	binary.Write(&wav, binary.LittleEndian, uint32(44100)) // sample rate
	binary.Write(&wav, binary.LittleEndian, uint32(88200))
	binary.Write(&wav, binary.LittleEndian, uint16(2))
	binary.Write(&wav, binary.LittleEndian, uint16(16))
	wav.WriteString("LIST")
	binary.Write(&wav, binary.LittleEndian, uint32(len(listBody)))
	wav.Write(listBody)
	if len(listBody)%2 == 1 {
		wav.WriteByte(0x00) // RIFF word-alignment byte
	}
	wav.WriteString("data")
	binary.Write(&wav, binary.LittleEndian, uint32(4))
	wav.Write([]byte{0, 0, 0, 0})
	out := wav.Bytes()
	binary.LittleEndian.PutUint32(out[4:], uint32(len(out)-8))
	return out
}

func readMetadataFromBytes(t *testing.T, b []byte) *Decoder {
	t.Helper()
	d := NewDecoder(bytes.NewReader(b))
	d.ReadMetadata()
	if err := d.Err(); err != nil {
		t.Fatalf("unexpected error reading metadata: %v", err)
	}
	return d
}

// The sample described in the bug report: odd-length strings, multiple
// unknown subchunks and a trailing single byte in the LIST/INFO chunk.
// Both word-aligned (padded) and packed (unpadded) subchunk layouts must
// parse: every known field preserved, unknown subchunks skipped and no
// spurious EOF error.
func TestDecodeListChunkOddValuesUnknownSubchunksTrailingByte(t *testing.T) {
	for _, tc := range []struct {
		desc string
		pad  bool
	}{
		{"word-aligned subchunks", true},
		{"packed subchunks", false},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			var body bytes.Buffer
			body.WriteString("INFO")
			body.Write(infoSubchunk("IART", []byte("odd artist\x00"), tc.pad)) // 11 bytes, odd
			body.Write(infoSubchunk("XXXX", []byte("unknown1\x00"), tc.pad))   // unknown, odd
			body.Write(infoSubchunk("YYYY", []byte("u2\x00"), tc.pad))         // unknown, odd
			body.Write(infoSubchunk("ISFT", []byte("some software\x00"), tc.pad))
			body.Write(infoSubchunk("ICMT", []byte("hi\x00"), tc.pad)) // 3 bytes, odd
			body.WriteByte(0x00)                                       // trailing single byte inside the chunk

			d := readMetadataFromBytes(t, wavWithList(t, body.Bytes()))
			if d.Metadata == nil {
				t.Fatal("expected metadata to be parsed")
			}
			if d.Metadata.Artist != "odd artist" {
				t.Errorf("Artist: expected %q, got %q", "odd artist", d.Metadata.Artist)
			}
			if d.Metadata.Software != "some software" {
				t.Errorf("Software: expected %q, got %q", "some software", d.Metadata.Software)
			}
			if d.Metadata.Comments != "hi" {
				t.Errorf("Comments: expected %q, got %q", "hi", d.Metadata.Comments)
			}
		})
	}
}

// A LIST chunk whose type isn't INFO (e.g. adtl) must be skipped without
// error and without populating metadata.
func TestDecodeListChunkNonINFO(t *testing.T) {
	var body bytes.Buffer
	body.WriteString("adtl")
	body.WriteString("whatever")
	d := readMetadataFromBytes(t, wavWithList(t, body.Bytes()))
	if d.Metadata != nil {
		t.Errorf("expected no metadata, got %#v", d.Metadata)
	}
}

// An empty LIST chunk keeps the historical error semantics: DecodeListChunk
// returns an error wrapping io.EOF, which ReadMetadata swallows.
func TestDecodeListChunkEmpty(t *testing.T) {
	d := &Decoder{}
	ch := &riff.Chunk{ID: CIDList, Size: 0, R: bytes.NewReader(nil)}
	err := DecodeListChunk(d, ch)
	if err == nil {
		t.Fatal("expected an error for an empty LIST chunk")
	}
	if !errors.Is(err, io.EOF) {
		t.Errorf("expected the error to wrap io.EOF, got %v", err)
	}

	// through the public decoding path, no error is reported
	dec := readMetadataFromBytes(t, wavWithList(t, nil))
	if dec.Metadata != nil {
		t.Errorf("expected no metadata, got %#v", dec.Metadata)
	}
}

// A subchunk declaring a corrupt (oversized) length must not fail nor
// allocate based on the declared size; previously parsed fields are kept.
func TestDecodeListChunkCorruptLength(t *testing.T) {
	var body bytes.Buffer
	body.WriteString("INFO")
	body.Write(infoSubchunk("IART", []byte("odd artist\x00"), true))
	body.WriteString("ISFT")
	binary.Write(&body, binary.LittleEndian, uint32(0x7fffffff)) // corrupt length
	body.WriteString("short\x00")

	d := readMetadataFromBytes(t, wavWithList(t, body.Bytes()))
	if d.Metadata == nil {
		t.Fatal("expected metadata to be parsed")
	}
	if d.Metadata.Artist != "odd artist" {
		t.Errorf("Artist: expected %q, got %q", "odd artist", d.Metadata.Artist)
	}
}

// Parsing a large number of subchunks stays linear: no re-copying of the
// chunk buffer per entry. 6000 mixed known/unknown, odd/even sized entries
// with a known field at the very end (a misalignment anywhere would lose it).
func TestDecodeListChunkManySubchunks(t *testing.T) {
	var body bytes.Buffer
	body.WriteString("INFO")
	body.Write(infoSubchunk("IART", []byte("first artist\x00"), true))
	const n = 6000
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("X%03d", i%1000)
		val := []byte(fmt.Sprintf("value-%d\x00", i))
		body.Write(infoSubchunk(id, val, true))
	}
	body.Write(infoSubchunk("ISFT", []byte("last software\x00"), true))

	d := readMetadataFromBytes(t, wavWithList(t, body.Bytes()))
	if d.Metadata == nil {
		t.Fatal("expected metadata to be parsed")
	}
	if d.Metadata.Artist != "first artist" {
		t.Errorf("Artist: expected %q, got %q", "first artist", d.Metadata.Artist)
	}
	if d.Metadata.Software != "last software" {
		t.Errorf("Software: expected %q, got %q", "last software", d.Metadata.Software)
	}
}

// Files written by this package's encoder pack odd-length INFO values
// without padding and store the LIST chunk last; decoding them must
// preserve every field.
func TestEncoderMetadataRoundTripOddLengthValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "odd-metadata.wav")
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	e := NewEncoder(out, 44100, 16, 1, 1)
	e.Metadata = &Metadata{
		Artist:   "odd artist", // 10 chars + NUL = 11 bytes, odd
		Software: "some software",
		Comments: "hi",
	}
	ib := &audio.IntBuffer{
		Data:           make([]int, 8),
		Format:         &audio.Format{NumChannels: 1, SampleRate: 44100},
		SourceBitDepth: 16,
	}
	if err := e.Write(ib); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	out.Close()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	d := readMetadataFromBytes(t, raw)
	if d.Metadata == nil {
		t.Fatal("expected metadata to be parsed")
	}
	if d.Metadata.Artist != "odd artist" {
		t.Errorf("Artist: expected %q, got %q", "odd artist", d.Metadata.Artist)
	}
	if d.Metadata.Software != "some software" {
		t.Errorf("Software: expected %q, got %q", "some software", d.Metadata.Software)
	}
	if d.Metadata.Comments != "hi" {
		t.Errorf("Comments: expected %q, got %q", "hi", d.Metadata.Comments)
	}
}
