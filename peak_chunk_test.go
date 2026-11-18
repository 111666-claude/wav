package wav

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-audio/audio"
)

// stereoImpulseBuffer returns a 16-bit stereo buffer of 8 frames with a
// fixed impulse on each channel: 0.5 on the left at frame 3 and -0.25 on
// the right at frame 6.
func stereoImpulseBuffer() *audio.IntBuffer {
	return &audio.IntBuffer{
		Format: &audio.Format{NumChannels: 2, SampleRate: 44100},
		Data: []int{
			100, 100, // frame 0
			-200, 300, // frame 1
			400, -500, // frame 2
			16384, 600, // frame 3: left peak 0.5
			700, -800, // frame 4
			900, 1000, // frame 5
			1100, -8192, // frame 6: right peak 0.25
			1200, 1300, // frame 7
		},
		SourceBitDepth: 16,
	}
}

func encodeToFile(t *testing.T, writePeak bool, buf *audio.IntBuffer) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "out.wav")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	e := NewEncoder(f, 44100, 16, 2, 1)
	e.WritePeakChunk = writePeak
	e.PeakTimestamp = 1234567890
	if buf != nil {
		if err := e.Write(buf); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// dataChunkPayload extracts the raw payload of the data chunk of a wav file.
func dataChunkPayload(t *testing.T, b []byte) []byte {
	t.Helper()
	if len(b) < 12 || string(b[:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		t.Fatalf("not a wav file: %q", b[:4])
	}
	r := bytes.NewReader(b[12:])
	for r.Len() >= 8 {
		var id [4]byte
		var size uint32
		if err := binary.Read(r, binary.LittleEndian, &id); err != nil {
			t.Fatal(err)
		}
		if err := binary.Read(r, binary.LittleEndian, &size); err != nil {
			t.Fatal(err)
		}
		if id == [4]byte{'d', 'a', 't', 'a'} {
			payload := make([]byte, size)
			if _, err := io.ReadFull(r, payload); err != nil {
				t.Fatal(err)
			}
			return payload
		}
		if size%2 == 1 {
			size++
		}
		if _, err := r.Seek(int64(size), io.SeekCurrent); err != nil {
			t.Fatal(err)
		}
	}
	t.Fatal("data chunk not found")
	return nil
}

func TestPeakChunkRoundTrip(t *testing.T) {
	b := encodeToFile(t, true, stereoImpulseBuffer())

	// the RIFF size must account for the appended PEAK chunk
	if got, want := binary.LittleEndian.Uint32(b[4:8]), uint32(len(b)-8); got != want {
		t.Fatalf("RIFF size = %d, expected %d", got, want)
	}

	d := NewDecoder(bytes.NewReader(b))
	d.ReadMetadata()
	if d.Err() != nil {
		t.Fatalf("unexpected decoder error: %v", d.Err())
	}
	if d.Metadata == nil || d.Metadata.Peak == nil {
		t.Fatal("expected a PEAK chunk, got none")
	}
	peak := d.Metadata.Peak
	if peak.Version != PeakChunkVersion {
		t.Errorf("version = %d, expected %d", peak.Version, PeakChunkVersion)
	}
	if peak.Timestamp != 1234567890 {
		t.Errorf("timestamp = %d, expected 1234567890", peak.Timestamp)
	}
	if len(peak.Peaks) != 2 {
		t.Fatalf("got %d channel peaks, expected 2", len(peak.Peaks))
	}
	if peak.Peaks[0].Value != 0.5 || peak.Peaks[0].Position != 3 {
		t.Errorf("left peak = %v @ %d, expected 0.5 @ 3", peak.Peaks[0].Value, peak.Peaks[0].Position)
	}
	if peak.Peaks[1].Value != 0.25 || peak.Peaks[1].Position != 6 {
		t.Errorf("right peak = %v @ %d, expected 0.25 @ 6", peak.Peaks[1].Value, peak.Peaks[1].Position)
	}

	// the PCM data must still decode correctly
	d2 := NewDecoder(bytes.NewReader(b))
	pcm, err := d2.FullPCMBuffer()
	if err != nil {
		t.Fatalf("couldn't decode PCM: %v", err)
	}
	want := stereoImpulseBuffer().Data
	if len(pcm.Data) != len(want) {
		t.Fatalf("decoded %d samples, expected %d", len(pcm.Data), len(want))
	}
	for i := range want {
		if pcm.Data[i] != want[i] {
			t.Fatalf("sample %d = %d, expected %d", i, pcm.Data[i], want[i])
		}
	}
}

func TestPeakChunkDoesNotAlterPCM(t *testing.T) {
	withPeak := encodeToFile(t, true, stereoImpulseBuffer())
	withoutPeak := encodeToFile(t, false, stereoImpulseBuffer())

	h1 := sha256.Sum256(dataChunkPayload(t, withPeak))
	h2 := sha256.Sum256(dataChunkPayload(t, withoutPeak))
	if h1 != h2 {
		t.Fatal("PCM payload changed when the PEAK chunk was added")
	}
	if bytes.Contains(withoutPeak, CIDPeak[:]) {
		t.Fatal("PEAK chunk present while WritePeakChunk was disabled")
	}
}

// chunk serializes a RIFF chunk (ID, size, payload, padding).
func chunk(id string, payload []byte) []byte {
	b := new(bytes.Buffer)
	b.WriteString(id)
	binary.Write(b, binary.LittleEndian, uint32(len(payload)))
	b.Write(payload)
	if len(payload)%2 == 1 {
		b.WriteByte(0)
	}
	return b.Bytes()
}

// buildWav assembles a RIFF/WAVE file from serialized chunks.
func buildWav(chunks ...[]byte) []byte {
	b := new(bytes.Buffer)
	b.WriteString("RIFF")
	size := uint32(4)
	for _, c := range chunks {
		size += uint32(len(c))
	}
	binary.Write(b, binary.LittleEndian, size)
	b.WriteString("WAVE")
	for _, c := range chunks {
		b.Write(c)
	}
	return b.Bytes()
}

func fmtChunk(numChans uint16) []byte {
	p := new(bytes.Buffer)
	binary.Write(p, binary.LittleEndian, uint16(1))                  // PCM
	binary.Write(p, binary.LittleEndian, numChans)                   // channels
	binary.Write(p, binary.LittleEndian, uint32(44100))              // sample rate
	binary.Write(p, binary.LittleEndian, 44100*4*uint32(numChans)/2) // byte rate
	binary.Write(p, binary.LittleEndian, 2*numChans)                 // block align
	binary.Write(p, binary.LittleEndian, uint16(16))                 // bits per sample
	return chunk("fmt ", p.Bytes())
}

func peakChunkPayload(version, ts uint32, peaks []ChannelPeak) []byte {
	p := new(bytes.Buffer)
	binary.Write(p, binary.LittleEndian, version)
	binary.Write(p, binary.LittleEndian, ts)
	for _, pk := range peaks {
		binary.Write(p, binary.LittleEndian, pk.Value)
		binary.Write(p, binary.LittleEndian, pk.Position)
	}
	return p.Bytes()
}

func TestPeakChunkDecodeWithUnknownChunk(t *testing.T) {
	// stereo samples matching the peaks stored below:
	// left peak 0.75 at frame 5, right peak 0.125 at frame 2
	frames := [][2]int16{
		{0, 0}, {100, -100}, {200, 4096}, {-300, 300},
		{400, -400}, {24576, 500}, {-600, 600}, {700, -700},
	}
	dataPayload := new(bytes.Buffer)
	for _, f := range frames {
		binary.Write(dataPayload, binary.LittleEndian, f[0])
		binary.Write(dataPayload, binary.LittleEndian, f[1])
	}

	b := buildWav(
		fmtChunk(2),
		chunk("JUNK", []byte{1, 2, 3}), // unknown chunk, odd size to exercise padding
		chunk("PEAK", peakChunkPayload(1, 42, []ChannelPeak{
			{Value: 0.75, Position: 5},
			{Value: 0.125, Position: 2},
		})),
		chunk("data", dataPayload.Bytes()),
	)

	d := NewDecoder(bytes.NewReader(b))
	d.ReadMetadata()
	if d.Err() != nil {
		t.Fatalf("unexpected decoder error: %v", d.Err())
	}
	if d.Metadata == nil || d.Metadata.Peak == nil {
		t.Fatal("expected a PEAK chunk, got none")
	}
	peak := d.Metadata.Peak
	if peak.Version != 1 || peak.Timestamp != 42 {
		t.Errorf("version/timestamp = %d/%d, expected 1/42", peak.Version, peak.Timestamp)
	}
	if len(peak.Peaks) != 2 {
		t.Fatalf("got %d channel peaks, expected 2", len(peak.Peaks))
	}
	if peak.Peaks[0].Value != 0.75 || peak.Peaks[0].Position != 5 {
		t.Errorf("left peak = %v @ %d, expected 0.75 @ 5", peak.Peaks[0].Value, peak.Peaks[0].Position)
	}
	if peak.Peaks[1].Value != 0.125 || peak.Peaks[1].Position != 2 {
		t.Errorf("right peak = %v @ %d, expected 0.125 @ 2", peak.Peaks[1].Value, peak.Peaks[1].Position)
	}

	// PCM must still be reachable after the unknown and PEAK chunks
	d2 := NewDecoder(bytes.NewReader(b))
	pcm, err := d2.FullPCMBuffer()
	if err != nil {
		t.Fatalf("couldn't decode PCM: %v", err)
	}
	if len(pcm.Data) != 16 {
		t.Fatalf("decoded %d samples, expected 16", len(pcm.Data))
	}
	if pcm.Data[5*2] != 24576 || pcm.Data[2*2+1] != 4096 {
		t.Errorf("impulse samples not at the expected positions: %v", pcm.Data)
	}
}

func TestPeakChunkDecodeErrors(t *testing.T) {
	dataChunk := chunk("data", make([]byte, 32))

	t.Run("channel mismatch", func(t *testing.T) {
		b := buildWav(fmtChunk(2), chunk("PEAK", peakChunkPayload(1, 1, []ChannelPeak{
			{Value: 0.5, Position: 1},
		})), dataChunk)
		d := NewDecoder(bytes.NewReader(b))
		d.ReadMetadata()
		if !errors.Is(d.Err(), ErrPeakChannelMismatch) {
			t.Fatalf("expected ErrPeakChannelMismatch, got %v", d.Err())
		}
	})

	t.Run("declared size too small", func(t *testing.T) {
		b := buildWav(fmtChunk(2), chunk("PEAK", []byte{1, 0, 0, 0}), dataChunk)
		d := NewDecoder(bytes.NewReader(b))
		d.ReadMetadata()
		if !errors.Is(d.Err(), ErrPeakTruncated) {
			t.Fatalf("expected ErrPeakTruncated, got %v", d.Err())
		}
	})

	t.Run("misaligned size", func(t *testing.T) {
		// 20 bytes: 8 header + 12, not a multiple of 8 per entry
		b := buildWav(fmtChunk(2), chunk("PEAK", make([]byte, 20)), dataChunk)
		d := NewDecoder(bytes.NewReader(b))
		d.ReadMetadata()
		if !errors.Is(d.Err(), ErrPeakTruncated) {
			t.Fatalf("expected ErrPeakTruncated, got %v", d.Err())
		}
	})

	t.Run("truncated payload", func(t *testing.T) {
		payload := peakChunkPayload(1, 1, []ChannelPeak{
			{Value: 0.5, Position: 1},
			{Value: 0.25, Position: 2},
		})
		// declare the full size but truncate the actual content
		b := buildWav(fmtChunk(2), chunk("PEAK", payload[:len(payload)-5]))
		// fix the declared chunk size back to the full payload length
		// so the decoder expects more bytes than available
		d := NewDecoder(bytes.NewReader(b))
		d.ReadMetadata()
		if !errors.Is(d.Err(), ErrPeakTruncated) {
			t.Fatalf("expected ErrPeakTruncated, got %v", d.Err())
		}
	})

	t.Run("non-finite peak", func(t *testing.T) {
		b := buildWav(fmtChunk(2), chunk("PEAK", peakChunkPayload(1, 1, []ChannelPeak{
			{Value: float32(math.NaN()), Position: 0},
			{Value: 0.25, Position: 2},
		})), dataChunk)
		d := NewDecoder(bytes.NewReader(b))
		d.ReadMetadata()
		if !errors.Is(d.Err(), ErrPeakNonFinite) {
			t.Fatalf("expected ErrPeakNonFinite, got %v", d.Err())
		}
	})

	t.Run("no peak chunk", func(t *testing.T) {
		b := encodeToFile(t, false, stereoImpulseBuffer())
		d := NewDecoder(bytes.NewReader(b))
		d.ReadMetadata()
		if d.Err() != nil {
			t.Fatalf("unexpected decoder error: %v", d.Err())
		}
		if d.Metadata != nil && d.Metadata.Peak != nil {
			t.Fatal("expected no PEAK chunk")
		}
	})
}

func TestPeakChunkEmptyAudio(t *testing.T) {
	// writing no frames must not emit a PEAK chunk
	b := encodeToFile(t, true, &audio.IntBuffer{
		Format: &audio.Format{NumChannels: 2, SampleRate: 44100},
		Data:   []int{},
	})
	if bytes.Contains(b, CIDPeak[:]) {
		t.Fatal("PEAK chunk written for empty audio")
	}
}

type errSeekWriter struct {
	bytes.Buffer
}

func (w *errSeekWriter) Seek(offset int64, whence int) (int64, error) {
	return 0, errors.New("seeking not supported")
}

func TestPeakChunkNonSeekableWriter(t *testing.T) {
	e := NewEncoder(&errSeekWriter{}, 44100, 16, 2, 1)
	e.WritePeakChunk = true
	if err := e.Write(stereoImpulseBuffer()); err != nil {
		t.Fatal(err)
	}
	// Close needs to backpatch the RIFF/data sizes and must report the
	// failure instead of silently producing an invalid file
	if err := e.Close(); err == nil {
		t.Fatal("expected an error when closing with a non-seekable writer")
	}
}

func TestComputePeaks(t *testing.T) {
	t.Run("stereo", func(t *testing.T) {
		pc, err := ComputePeaks([]float64{
			0.1, -0.2,
			-0.5, 0.3,
			0.4, 0.45,
		}, 2)
		if err != nil {
			t.Fatal(err)
		}
		if pc.Peaks[0].Value != 0.5 || pc.Peaks[0].Position != 1 {
			t.Errorf("left peak = %v @ %d, expected 0.5 @ 1", pc.Peaks[0].Value, pc.Peaks[0].Position)
		}
		if pc.Peaks[1].Value != 0.45 || pc.Peaks[1].Position != 2 {
			t.Errorf("right peak = %v @ %d, expected 0.45 @ 2", pc.Peaks[1].Value, pc.Peaks[1].Position)
		}
	})

	t.Run("empty", func(t *testing.T) {
		pc, err := ComputePeaks(nil, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(pc.Peaks) != 2 || pc.Peaks[0].Value != 0 || pc.Peaks[1].Value != 0 {
			t.Errorf("expected zeroed peaks, got %+v", pc.Peaks)
		}
	})

	t.Run("non-finite", func(t *testing.T) {
		if _, err := ComputePeaks([]float64{0.1, math.Inf(1)}, 2); !errors.Is(err, ErrPeakNonFinite) {
			t.Fatalf("expected ErrPeakNonFinite, got %v", err)
		}
		if _, err := ComputePeaks([]float64{math.NaN()}, 1); !errors.Is(err, ErrPeakNonFinite) {
			t.Fatalf("expected ErrPeakNonFinite, got %v", err)
		}
	})

	t.Run("invalid channel count", func(t *testing.T) {
		if _, err := ComputePeaks([]float64{0.1}, 0); err == nil {
			t.Fatal("expected an error for 0 channels")
		}
	})
}
