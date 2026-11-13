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

	"github.com/go-audio/audio"
)

// wrappedEOFReader simulates a network buffer sitting in front of the WAV
// stream: instead of returning a bare io.EOF, it wraps it. Like many
// streaming readers it may also return the final bytes of the stream
// together with the (wrapped) io.EOF in a single Read call.
type wrappedEOFReader struct {
	r *bytes.Reader
}

func (e *wrappedEOFReader) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err == io.EOF {
		return n, fmt.Errorf("net buffer: %w", io.EOF)
	}
	if n > 0 && e.r.Len() == 0 {
		// final bytes of the stream delivered with the EOF marker
		return n, fmt.Errorf("net buffer: %w", io.EOF)
	}
	return n, err
}

func (e *wrappedEOFReader) Seek(offset int64, whence int) (int64, error) {
	return e.r.Seek(offset, whence)
}

func wrappedEOFDecoder(t *testing.T, path string) *Decoder {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return NewDecoder(&wrappedEOFReader{r: bytes.NewReader(b)})
}

func plainDecoder(t *testing.T, path string) *Decoder {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return NewDecoder(bytes.NewReader(b))
}

// testWavBytes builds a minimal 16-bit mono WAV holding the given samples.
func testWavBytes(samples []int) []byte {
	buf := &bytes.Buffer{}
	dataSize := uint32(len(samples) * 2)
	buf.WriteString("RIFF")
	binary.Write(buf, binary.LittleEndian, uint32(36)+dataSize)
	buf.WriteString("WAVE")
	buf.WriteString("fmt ")
	binary.Write(buf, binary.LittleEndian, uint32(16))
	binary.Write(buf, binary.LittleEndian, uint16(1))    // PCM
	binary.Write(buf, binary.LittleEndian, uint16(1))    // mono
	binary.Write(buf, binary.LittleEndian, uint32(8000)) // sample rate
	binary.Write(buf, binary.LittleEndian, uint32(16000))
	binary.Write(buf, binary.LittleEndian, uint16(2))
	binary.Write(buf, binary.LittleEndian, uint16(16)) // bit depth
	buf.WriteString("data")
	binary.Write(buf, binary.LittleEndian, dataSize)
	for _, s := range samples {
		binary.Write(buf, binary.LittleEndian, int16(s))
	}
	return buf.Bytes()
}

// assertStatusStable verifies that repeated status queries are pure:
// they never change the reported outcome.
func assertStatusStable(t *testing.T, d *Decoder, wantEOF bool, wantErr error) {
	t.Helper()
	for i := 0; i < 3; i++ {
		if got := d.EOF(); got != wantEOF {
			t.Fatalf("EOF() call %d = %v, expected %v", i, got, wantEOF)
		}
		if got := d.Err(); got != wantErr {
			t.Fatalf("Err() call %d = %v, expected %v", i, got, wantErr)
		}
	}
}

func TestDecoderWrappedEOF_ReadMetadata(t *testing.T) {
	path := "fixtures/listinfo.wav"
	plain := plainDecoder(t, path)
	plain.ReadMetadata()
	if plain.Err() != nil {
		t.Fatalf("baseline decode failed: %v", plain.Err())
	}

	d := wrappedEOFDecoder(t, path)
	d.ReadMetadata()
	if err := d.Err(); err != nil {
		t.Fatalf("expected no error after a clean wrapped EOF, got %v", err)
	}
	if !reflect.DeepEqual(plain.Metadata, d.Metadata) {
		t.Fatalf("metadata mismatch:\nplain:  %+v\nwrapped: %+v", plain.Metadata, d.Metadata)
	}
	if !d.EOF() {
		t.Fatal("expected EOF() to report the end of the stream")
	}
	assertStatusStable(t, d, true, nil)
}

func TestDecoderWrappedEOF_FullPCMBuffer(t *testing.T) {
	path := "fixtures/kick.wav"
	plain := plainDecoder(t, path)
	want, err := plain.FullPCMBuffer()
	if err != nil {
		t.Fatalf("baseline decode failed: %v", err)
	}

	d := wrappedEOFDecoder(t, path)
	buf, err := d.FullPCMBuffer()
	if err != nil {
		t.Fatalf("expected no error after a clean wrapped EOF, got %v", err)
	}
	if !reflect.DeepEqual(want.Data, buf.Data) {
		t.Fatalf("PCM data mismatch: baseline has %d samples, wrapped decode has %d",
			len(want.Data), len(buf.Data))
	}
	if !d.EOF() {
		t.Fatal("expected EOF() to report the end of the stream")
	}
	assertStatusStable(t, d, true, nil)
}

func TestDecoderWrappedEOF_PCMBuffer(t *testing.T) {
	path := "fixtures/kick.wav"
	plain := plainDecoder(t, path)
	full, err := plain.FullPCMBuffer()
	if err != nil {
		t.Fatalf("baseline decode failed: %v", err)
	}

	for _, size := range []int{1, 7, 255} {
		t.Run(fmt.Sprintf("buffer size %d", size), func(t *testing.T) {
			d := wrappedEOFDecoder(t, path)
			buf := &audio.IntBuffer{Data: make([]int, size)}
			var got []int
			for {
				n, err := d.PCMBuffer(buf)
				if err != nil {
					t.Fatalf("PCMBuffer failed after %d samples: %v", len(got), err)
				}
				if n == 0 {
					break
				}
				got = append(got, buf.Data[:n]...)
			}
			if !reflect.DeepEqual(full.Data, got) {
				t.Fatalf("segmented decode mismatch: expected %d samples, got %d",
					len(full.Data), len(got))
			}
			if !d.EOF() {
				t.Fatal("expected EOF() to report the end of the stream")
			}
			assertStatusStable(t, d, true, nil)
		})
	}
}

// failAfterReader returns a wrapped non-EOF error once the read cursor
// reaches the configured offset.
type failAfterReader struct {
	r     *bytes.Reader
	after int64
	err   error
}

func (f *failAfterReader) Read(p []byte) (int, error) {
	pos, err := f.r.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	if pos >= f.after {
		return 0, fmt.Errorf("net buffer: %w", f.err)
	}
	return f.r.Read(p)
}

func (f *failAfterReader) Seek(offset int64, whence int) (int64, error) {
	return f.r.Seek(offset, whence)
}

func TestDecoderWrappedNonEOFError(t *testing.T) {
	errBoom := errors.New("connection reset")
	samples := make([]int, 64)
	for i := range samples {
		samples[i] = i
	}
	data := testWavBytes(samples)

	t.Run("chunk scan", func(t *testing.T) {
		// fail right after the fmt chunk, while scanning for the data chunk
		d := NewDecoder(&failAfterReader{r: bytes.NewReader(data), after: 36, err: errBoom})
		err := d.FwdToPCM()
		if !errors.Is(err, errBoom) {
			t.Fatalf("expected the wrapped error to stay traceable, got %v", err)
		}
		if !errors.Is(d.Err(), errBoom) {
			t.Fatalf("expected Err() to stay traceable, got %v", d.Err())
		}
		if d.EOF() {
			t.Fatal("a non-EOF error must not be reported as EOF")
		}
	})

	t.Run("pcm read", func(t *testing.T) {
		// fail once the PCM data itself is being read (data starts at byte 44)
		d := NewDecoder(&failAfterReader{r: bytes.NewReader(data), after: 44, err: errBoom})
		buf := &audio.IntBuffer{Data: make([]int, 8)}
		_, err := d.PCMBuffer(buf)
		if !errors.Is(err, errBoom) {
			t.Fatalf("expected the wrapped error to stay traceable, got %v", err)
		}
		if !errors.Is(d.Err(), errBoom) {
			t.Fatalf("expected Err() to stay traceable, got %v", d.Err())
		}
		if d.EOF() {
			t.Fatal("a non-EOF error must not be reported as EOF")
		}
	})

	t.Run("metadata scan", func(t *testing.T) {
		d := NewDecoder(&failAfterReader{r: bytes.NewReader(data), after: 36, err: errBoom})
		d.ReadMetadata()
		if !errors.Is(d.Err(), errBoom) {
			t.Fatalf("expected Err() to stay traceable, got %v", d.Err())
		}
		if d.EOF() {
			t.Fatal("a non-EOF error must not be reported as EOF")
		}
	})
}

func TestDecoderWrappedEOF_EmptyInput(t *testing.T) {
	newDecoder := func() *Decoder {
		return NewDecoder(&wrappedEOFReader{r: bytes.NewReader(nil)})
	}

	if newDecoder().IsValidFile() {
		t.Fatal("empty input must not be reported as a valid file")
	}

	d := newDecoder()
	if _, err := d.FullPCMBuffer(); err == nil {
		t.Fatal("empty input must not decode as a valid PCM stream")
	}

	d = newDecoder()
	if _, err := d.PCMBuffer(&audio.IntBuffer{Data: make([]int, 8)}); err == nil {
		t.Fatal("empty input must not decode as a valid PCM stream")
	}
}

func TestDecoderWrappedEOF_Truncated(t *testing.T) {
	samples := make([]int, 100)
	for i := range samples {
		samples[i] = i * 3
	}
	full := testWavBytes(samples)
	// cut the stream in the middle of the PCM data (data starts at byte 44)
	truncated := full[:44+100]

	t.Run("full pcm", func(t *testing.T) {
		d := NewDecoder(&wrappedEOFReader{r: bytes.NewReader(truncated)})
		buf, err := d.FullPCMBuffer()
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("expected the truncation to surface as ErrUnexpectedEOF, got %v", err)
		}
		if got := len(buf.Data); got != 50 {
			t.Fatalf("expected the 50 complete samples to be decoded, got %d", got)
		}
		if err := d.Err(); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("expected Err() to report the truncation, got %v", err)
		}
	})

	t.Run("segmented pcm", func(t *testing.T) {
		d := NewDecoder(&wrappedEOFReader{r: bytes.NewReader(truncated)})
		buf := &audio.IntBuffer{Data: make([]int, 8)}
		total := 0
		var err error
		for err == nil {
			var n int
			n, err = d.PCMBuffer(buf)
			total += n
		}
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("expected the truncation to surface as ErrUnexpectedEOF, got %v", err)
		}
		if total != 50 {
			t.Fatalf("expected the 50 complete samples to be decoded, got %d", total)
		}
	})

	t.Run("truncated header", func(t *testing.T) {
		d := NewDecoder(&wrappedEOFReader{r: bytes.NewReader(full[:30])})
		if d.IsValidFile() {
			t.Fatal("a file truncated in its header must not be valid")
		}
	})
}

// TestDecoderWrappedEOF_ManySmallChunks reads a stream made of many small
// PCM buffers to make sure the work stays linear in the number of reads.
func TestDecoderWrappedEOF_ManySmallChunks(t *testing.T) {
	const count = 6000
	samples := make([]int, count)
	for i := range samples {
		samples[i] = (i*37)%2000 - 1000
	}
	d := NewDecoder(&wrappedEOFReader{r: bytes.NewReader(testWavBytes(samples))})

	buf := &audio.IntBuffer{Data: make([]int, 1)}
	for i := 0; i < count; i++ {
		n, err := d.PCMBuffer(buf)
		if err != nil {
			t.Fatalf("chunk %d: unexpected error: %v", i, err)
		}
		if n != 1 {
			t.Fatalf("chunk %d: expected 1 sample, got %d", i, n)
		}
		if buf.Data[0] != samples[i] {
			t.Fatalf("chunk %d: expected sample %d, got %d", i, samples[i], buf.Data[0])
		}
	}
	if n, err := d.PCMBuffer(buf); n != 0 || err != nil {
		t.Fatalf("expected a clean end after %d chunks, got n=%d err=%v", count, n, err)
	}
	if !d.EOF() {
		t.Fatal("expected EOF() to report the end of the stream")
	}
	assertStatusStable(t, d, true, nil)
}
