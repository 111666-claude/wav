package wav

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"reflect"
	"testing"

	"github.com/go-audio/audio"
)

// memWriteSeeker is an in-memory io.WriteSeeker used to exercise the encoder
// without touching the filesystem.
type memWriteSeeker struct {
	buf []byte
	pos int
}

func (m *memWriteSeeker) Write(p []byte) (int, error) {
	if m.pos+len(p) > len(m.buf) {
		newBuf := make([]byte, m.pos+len(p))
		copy(newBuf, m.buf)
		m.buf = newBuf
	}
	copy(m.buf[m.pos:], p)
	m.pos += len(p)
	return len(p), nil
}

func (m *memWriteSeeker) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = int64(m.pos) + offset
	case io.SeekEnd:
		abs = int64(len(m.buf)) + offset
	}
	if abs < 0 {
		return 0, fmt.Errorf("can't seek to a negative position")
	}
	m.pos = int(abs)
	return abs, nil
}

type walkedChunk struct {
	id      [4]byte
	size    uint32
	dataOff int // offset of the chunk content in the file
	headOff int // offset of the chunk ID in the file
}

// walkChunks parses the RIFF structure of a wav file and returns its chunks.
// It verifies the word alignment of every chunk and the total file size.
func walkChunks(t *testing.T, data []byte) []walkedChunk {
	t.Helper()
	if len(data) < 12 {
		t.Fatalf("file too small to be a wav file: %d bytes", len(data))
	}
	if string(data[:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		t.Fatalf("missing RIFF/WAVE headers")
	}
	if riffSize := binary.LittleEndian.Uint32(data[4:8]); int(riffSize) != len(data)-8 {
		t.Fatalf("RIFF size header says %d but the file holds %d bytes after the header", riffSize, len(data)-8)
	}
	var chunks []walkedChunk
	off := 12
	for off+8 <= len(data) {
		var id [4]byte
		copy(id[:], data[off:off+4])
		size := binary.LittleEndian.Uint32(data[off+4 : off+8])
		chunks = append(chunks, walkedChunk{id: id, size: size, dataOff: off + 8, headOff: off})
		off += 8 + int(size)
		if size%2 == 1 {
			// word alignment padding byte
			if off >= len(data) {
				t.Fatalf("chunk %q declares an odd size but no padding byte follows", id)
			}
			if data[off] != 0 {
				t.Fatalf("padding byte after chunk %q is not zero", id)
			}
			off++
		}
	}
	if off != len(data) {
		t.Fatalf("walking the chunks ended at offset %d, file is %d bytes", off, len(data))
	}
	return chunks
}

func chunkIDs(chunks []walkedChunk) string {
	var ids [][]byte
	for _, c := range chunks {
		ids = append(ids, append([]byte(nil), c.id[:]...))
	}
	return string(bytes.Join(ids, []byte(",")))
}

func TestEncoderCueAndSamplerRoundTrip(t *testing.T) {
	os.Mkdir("testOutput", 0777)
	cueID1 := [4]byte{1, 0, 0, 0}
	cueID2 := [4]byte{2, 0, 0, 0}
	cueID3 := [4]byte{3, 0, 0, 0}

	testCases := []struct {
		in  string
		out string
	}{
		{"fixtures/kick.wav", "testOutput/marked-kick.wav"},                 // 1 ch, 16 bits
		{"fixtures/kick-16b441k.wav", "testOutput/marked-kick-16b441k.wav"}, // 2 ch, 16 bits
	}

	for _, tc := range testCases {
		t.Run(path.Base(tc.in), func(t *testing.T) {
			in, err := os.Open(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			d := NewDecoder(in)
			buf, err := d.FullPCMBuffer()
			if err != nil {
				t.Fatalf("couldn't read buffer %s %v", tc.in, err)
			}
			in.Close()

			frames := buf.NumFrames()
			if frames < 4 {
				t.Fatalf("fixture %s too short for this test: %d frames", tc.in, frames)
			}

			out, err := os.Create(tc.out)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := os.Remove(tc.out); err != nil {
					panic(err)
				}
			}()

			e := NewEncoder(out, buf.Format.SampleRate, int(d.BitDepth), buf.Format.NumChannels, int(d.WavAudioFormat))
			e.Metadata = &Metadata{Artist: "go-audio"}
			if err := e.Write(buf); err != nil {
				t.Fatal(err)
			}
			offsets := []uint32{0, uint32(frames / 2), uint32(frames - 1)}
			for i, id := range [][4]byte{cueID1, cueID2, cueID3} {
				if err := e.AddCuePoint(id, offsets[i]); err != nil {
					t.Fatalf("failed to add cue point %d: %v", i, err)
				}
			}
			e.Metadata.SamplerInfo = &SamplerInfo{
				SamplePeriod:      22676,
				MIDIUnityNote:     60,
				MIDIPitchFraction: 0x80000000,
			}
			loops := []*SampleLoop{
				{CuePointID: cueID1, Type: 0, Start: 0, End: uint32(frames / 2), PlayCount: 3},
				{CuePointID: cueID3, Type: 1, Start: uint32(frames / 4), End: uint32(frames - 1), PlayCount: 0},
			}
			for _, l := range loops {
				if err := e.AddSampleLoop(l); err != nil {
					t.Fatalf("failed to add sample loop: %v", err)
				}
			}
			if err := e.Close(); err != nil {
				t.Fatal(err)
			}
			out.Close()

			nf, err := os.Open(tc.out)
			if err != nil {
				t.Fatal(err)
			}
			defer nf.Close()
			nd := NewDecoder(nf)
			nBuf, err := nd.FullPCMBuffer()
			if err != nil {
				t.Fatalf("couldn't extract the PCM from %s - %v", nf.Name(), err)
			}

			// the audio data must be untouched
			if !reflect.DeepEqual(buf.Data, nBuf.Data) {
				t.Fatalf("the PCM data didn't survive the marker round trip")
			}

			nd.ReadMetadata()
			if nd.Err() != nil {
				t.Fatal(nd.Err())
			}
			if nd.Metadata == nil {
				t.Fatal("expected metadata, got nil")
			}

			// existing INFO metadata must be preserved
			if nd.Metadata.Artist != "go-audio" {
				t.Errorf("expected Artist to be go-audio, got %q", nd.Metadata.Artist)
			}

			expectedCues := []*CuePoint{
				{ID: cueID1, DataChunkID: dataChunkID, SampleOffset: offsets[0]},
				{ID: cueID2, DataChunkID: dataChunkID, SampleOffset: offsets[1]},
				{ID: cueID3, DataChunkID: dataChunkID, SampleOffset: offsets[2]},
			}
			if !reflect.DeepEqual(expectedCues, nd.Metadata.CuePoints) {
				t.Errorf("cue points didn't round trip\nexpected: %#v\ngot:      %#v", expectedCues, nd.Metadata.CuePoints)
			}

			expectedSampler := &SamplerInfo{
				SamplePeriod:      22676,
				MIDIUnityNote:     60,
				MIDIPitchFraction: 0x80000000,
				NumSampleLoops:    2,
				Loops:             loops,
			}
			if !reflect.DeepEqual(expectedSampler, nd.Metadata.SamplerInfo) {
				t.Errorf("sampler info didn't round trip\nexpected: %#v\ngot:      %#v", expectedSampler, nd.Metadata.SamplerInfo)
			}
		})
	}
}

func TestEncoderMarkerChunkLayout(t *testing.T) {
	cueID1 := [4]byte{1, 0, 0, 0}
	cueID2 := [4]byte{2, 0, 0, 0}

	encode := func(t *testing.T, bitDepth, frames int, metadata *Metadata) []byte {
		t.Helper()
		ws := &memWriteSeeker{}
		e := NewEncoder(ws, 44100, bitDepth, 1, 1)
		buf := &audio.IntBuffer{
			Data:   make([]int, frames),
			Format: &audio.Format{NumChannels: 1, SampleRate: 44100},
		}
		if err := e.Write(buf); err != nil {
			t.Fatal(err)
		}
		e.Metadata = metadata
		if err := e.Close(); err != nil {
			t.Fatal(err)
		}
		return ws.buf
	}

	t.Run("even sizes", func(t *testing.T) {
		m := &Metadata{
			Artist: "go-audio",
			CuePoints: []*CuePoint{
				{ID: cueID1, DataChunkID: dataChunkID, SampleOffset: 0},
				{ID: cueID2, DataChunkID: dataChunkID, SampleOffset: 999},
			},
			SamplerInfo: &SamplerInfo{
				SamplePeriod:  22676,
				MIDIUnityNote: 60,
				Loops: []*SampleLoop{
					{CuePointID: cueID2, Type: 0, Start: 10, End: 999, PlayCount: 7},
				},
			},
		}
		data := encode(t, 16, 1000, m)
		chunks := walkChunks(t, data)
		if got := chunkIDs(chunks); got != "fmt ,data,cue ,smpl,LIST" {
			t.Fatalf("unexpected chunk layout: %s", got)
		}
		cue := chunks[2]
		if cue.size != 4+24*2 {
			t.Errorf("expected a cue chunk size of %d, got %d", 4+24*2, cue.size)
		}
		if n := binary.LittleEndian.Uint32(data[cue.dataOff:]); n != 2 {
			t.Errorf("expected 2 cue points in the cue chunk, got %d", n)
		}
		// second cue point sample offset
		if off := binary.LittleEndian.Uint32(data[cue.dataOff+4+24+20:]); off != 999 {
			t.Errorf("expected the second cue point at sample 999, got %d", off)
		}
		smpl := chunks[3]
		if smpl.size != 36+24*1 {
			t.Errorf("expected a smpl chunk size of %d, got %d", 36+24*1, smpl.size)
		}
		if n := binary.LittleEndian.Uint32(data[smpl.dataOff+28:]); n != 1 {
			t.Errorf("expected 1 sample loop in the smpl chunk, got %d", n)
		}
		if pc := binary.LittleEndian.Uint32(data[smpl.dataOff+36+20:]); pc != 7 {
			t.Errorf("expected a play count of 7, got %d", pc)
		}
	})

	t.Run("odd data chunk gets padded", func(t *testing.T) {
		// 101 frames of 8-bit mono -> 101 bytes of PCM data, an odd size.
		m := &Metadata{
			CuePoints: []*CuePoint{{ID: cueID1, DataChunkID: dataChunkID, SampleOffset: 100}},
		}
		data := encode(t, 8, 101, m)
		chunks := walkChunks(t, data)
		if got := chunkIDs(chunks); got != "fmt ,data,cue ,LIST" {
			t.Fatalf("unexpected chunk layout: %s", got)
		}
		dataChunk := chunks[1]
		if dataChunk.size != 101 {
			t.Fatalf("expected a data chunk size of 101, got %d", dataChunk.size)
		}
		if chunks[2].headOff%2 != 0 {
			t.Errorf("the cue chunk doesn't start on an even offset")
		}
		// the markers must still be decodable
		d := NewDecoder(bytes.NewReader(data))
		d.ReadMetadata()
		if d.Err() != nil {
			t.Fatal(d.Err())
		}
		if d.Metadata == nil || len(d.Metadata.CuePoints) != 1 {
			t.Fatalf("expected 1 cue point to be decoded, got %#v", d.Metadata)
		}
		if d.Metadata.CuePoints[0].SampleOffset != 100 {
			t.Errorf("expected a cue point at sample 100, got %d", d.Metadata.CuePoints[0].SampleOffset)
		}
	})
}

func TestEncoderMarkerErrors(t *testing.T) {
	cueID1 := [4]byte{1, 0, 0, 0}
	cueID2 := [4]byte{2, 0, 0, 0}

	newEncoder := func(t *testing.T, frames int) (*Encoder, *memWriteSeeker) {
		t.Helper()
		ws := &memWriteSeeker{}
		e := NewEncoder(ws, 44100, 16, 1, 1)
		if frames > 0 {
			buf := &audio.IntBuffer{
				Data:   make([]int, frames),
				Format: &audio.Format{NumChannels: 1, SampleRate: 44100},
			}
			if err := e.Write(buf); err != nil {
				t.Fatal(err)
			}
		}
		return e, ws
	}

	t.Run("duplicate cue point ID at add time", func(t *testing.T) {
		e, _ := newEncoder(t, 100)
		if err := e.AddCuePoint(cueID1, 0); err != nil {
			t.Fatal(err)
		}
		if err := e.AddCuePoint(cueID1, 50); !errors.Is(err, ErrDuplicateCuePointID) {
			t.Fatalf("expected ErrDuplicateCuePointID, got %v", err)
		}
	})

	t.Run("duplicate cue point ID at close time", func(t *testing.T) {
		e, _ := newEncoder(t, 100)
		e.Metadata = &Metadata{CuePoints: []*CuePoint{
			{ID: cueID1, SampleOffset: 0},
			{ID: cueID1, SampleOffset: 50},
		}}
		if err := e.Close(); !errors.Is(err, ErrDuplicateCuePointID) {
			t.Fatalf("expected ErrDuplicateCuePointID, got %v", err)
		}
	})

	t.Run("cue point out of range", func(t *testing.T) {
		e, ws := newEncoder(t, 100)
		if err := e.AddCuePoint(cueID1, 100); err != nil {
			t.Fatal(err)
		}
		if err := e.Close(); !errors.Is(err, ErrSampleOutOfRange) {
			t.Fatalf("expected ErrSampleOutOfRange, got %v", err)
		}
		if bytes.Contains(ws.buf, []byte("cue ")) {
			t.Fatal("a cue chunk was written even though the validation failed")
		}
	})

	t.Run("cue point without any audio data", func(t *testing.T) {
		e, _ := newEncoder(t, 0)
		if err := e.AddCuePoint(cueID1, 0); err != nil {
			t.Fatal(err)
		}
		if err := e.Close(); !errors.Is(err, ErrSampleOutOfRange) {
			t.Fatalf("expected ErrSampleOutOfRange, got %v", err)
		}
	})

	t.Run("reversed loop at add time", func(t *testing.T) {
		e, _ := newEncoder(t, 100)
		err := e.AddSampleLoop(&SampleLoop{CuePointID: cueID1, Start: 90, End: 10})
		if !errors.Is(err, ErrInvalidLoopRange) {
			t.Fatalf("expected ErrInvalidLoopRange, got %v", err)
		}
	})

	t.Run("reversed loop at close time", func(t *testing.T) {
		e, _ := newEncoder(t, 100)
		e.Metadata = &Metadata{
			CuePoints:   []*CuePoint{{ID: cueID1, SampleOffset: 0}},
			SamplerInfo: &SamplerInfo{Loops: []*SampleLoop{{CuePointID: cueID1, Start: 90, End: 10}}},
		}
		if err := e.Close(); !errors.Is(err, ErrInvalidLoopRange) {
			t.Fatalf("expected ErrInvalidLoopRange, got %v", err)
		}
	})

	t.Run("loop referencing an unknown cue point", func(t *testing.T) {
		e, _ := newEncoder(t, 100)
		if err := e.AddCuePoint(cueID1, 0); err != nil {
			t.Fatal(err)
		}
		if err := e.AddSampleLoop(&SampleLoop{CuePointID: cueID2, Start: 0, End: 99}); err != nil {
			t.Fatal(err)
		}
		if err := e.Close(); !errors.Is(err, ErrLoopCuePointNotFound) {
			t.Fatalf("expected ErrLoopCuePointNotFound, got %v", err)
		}
	})

	t.Run("loop ending past the audio data", func(t *testing.T) {
		e, _ := newEncoder(t, 100)
		if err := e.AddCuePoint(cueID1, 0); err != nil {
			t.Fatal(err)
		}
		if err := e.AddSampleLoop(&SampleLoop{CuePointID: cueID1, Start: 0, End: 100}); err != nil {
			t.Fatal(err)
		}
		if err := e.Close(); !errors.Is(err, ErrSampleOutOfRange) {
			t.Fatalf("expected ErrSampleOutOfRange, got %v", err)
		}
	})

	t.Run("nil loop", func(t *testing.T) {
		e, _ := newEncoder(t, 100)
		if err := e.AddSampleLoop(nil); err == nil {
			t.Fatal("expected an error when adding a nil loop")
		}
	})

	t.Run("failed close can be corrected", func(t *testing.T) {
		e, _ := newEncoder(t, 100)
		if err := e.AddCuePoint(cueID1, 500); err != nil {
			t.Fatal(err)
		}
		if err := e.Close(); !errors.Is(err, ErrSampleOutOfRange) {
			t.Fatalf("expected ErrSampleOutOfRange, got %v", err)
		}
		// fix the marker and close again, the file must come out valid
		e.Metadata.CuePoints[0].SampleOffset = 50
		if err := e.Close(); err != nil {
			t.Fatalf("expected the corrected close to succeed, got %v", err)
		}
	})

	t.Run("writes after close", func(t *testing.T) {
		e, _ := newEncoder(t, 100)
		if err := e.Close(); err != nil {
			t.Fatal(err)
		}
		if err := e.Close(); !errors.Is(err, ErrEncoderClosed) {
			t.Fatalf("expected ErrEncoderClosed on a second close, got %v", err)
		}
		if err := e.AddCuePoint(cueID1, 0); !errors.Is(err, ErrEncoderClosed) {
			t.Fatalf("expected ErrEncoderClosed, got %v", err)
		}
		if err := e.AddSampleLoop(&SampleLoop{Start: 0, End: 1}); !errors.Is(err, ErrEncoderClosed) {
			t.Fatalf("expected ErrEncoderClosed, got %v", err)
		}
		buf := &audio.IntBuffer{Data: make([]int, 10), Format: &audio.Format{NumChannels: 1, SampleRate: 44100}}
		if err := e.Write(buf); !errors.Is(err, ErrEncoderClosed) {
			t.Fatalf("expected ErrEncoderClosed, got %v", err)
		}
		if err := e.WriteFrame(1); !errors.Is(err, ErrEncoderClosed) {
			t.Fatalf("expected ErrEncoderClosed, got %v", err)
		}
	})
}

func TestEncoderNoMarkersCompat(t *testing.T) {
	newBuffer := func(frames int) *audio.IntBuffer {
		return &audio.IntBuffer{
			Data:   make([]int, frames),
			Format: &audio.Format{NumChannels: 1, SampleRate: 44100},
		}
	}

	t.Run("no metadata", func(t *testing.T) {
		ws := &memWriteSeeker{}
		e := NewEncoder(ws, 44100, 16, 1, 1)
		if err := e.Write(newBuffer(100)); err != nil {
			t.Fatal(err)
		}
		if err := e.Close(); err != nil {
			t.Fatal(err)
		}
		chunks := walkChunks(t, ws.buf)
		if got := chunkIDs(chunks); got != "fmt ,data" {
			t.Fatalf("expected only fmt and data chunks, got %s", got)
		}
		d := NewDecoder(bytes.NewReader(ws.buf))
		buf, err := d.FullPCMBuffer()
		if err != nil {
			t.Fatal(err)
		}
		if len(buf.Data) != 100 {
			t.Fatalf("expected 100 samples back, got %d", len(buf.Data))
		}
	})

	t.Run("metadata without markers", func(t *testing.T) {
		ws := &memWriteSeeker{}
		e := NewEncoder(ws, 44100, 16, 1, 1)
		if err := e.Write(newBuffer(100)); err != nil {
			t.Fatal(err)
		}
		e.Metadata = &Metadata{Artist: "go-audio"}
		if err := e.Close(); err != nil {
			t.Fatal(err)
		}
		chunks := walkChunks(t, ws.buf)
		if got := chunkIDs(chunks); got != "fmt ,data,LIST" {
			t.Fatalf("expected fmt, data and LIST chunks, got %s", got)
		}
		d := NewDecoder(bytes.NewReader(ws.buf))
		if _, err := d.FullPCMBuffer(); err != nil {
			t.Fatal(err)
		}
		d.ReadMetadata()
		if d.Metadata == nil || d.Metadata.Artist != "go-audio" {
			t.Fatalf("expected the artist to round trip, got %#v", d.Metadata)
		}
		if len(d.Metadata.CuePoints) != 0 {
			t.Fatalf("expected no cue points, got %#v", d.Metadata.CuePoints)
		}
	})
}
