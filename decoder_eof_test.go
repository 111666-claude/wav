package wav

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/go-audio/audio"
)

// errNetworkRead is a non-EOF error returned by a flaky network reader.
// It must stay traceable through errors.Is rather than being swallowed.
var errNetworkRead = errors.New("simulated network read failure")

// networkReadSeeker mimics a buffered network reader. When the underlying
// data is fully consumed it returns the final bytes together with a wrapped
// io.EOF (as allowed by the io.Reader contract) and keeps returning that
// error on every subsequent read. A non-EOF failure can be injected instead.
type networkReadSeeker struct {
	data      []byte
	pos       int64
	readCalls int
	// failAt, when > 0, makes the read reaching byte offset failAt return
	// failErr instead of the remaining data. The zero value disables it.
	failAt  int64
	failErr error
}

func (n *networkReadSeeker) Read(p []byte) (int, error) {
	n.readCalls++
	if n.failAt > 0 && n.pos >= n.failAt {
		return 0, n.failErr
	}
	if n.pos >= int64(len(n.data)) {
		return 0, fmt.Errorf("network stream closed: %w", io.EOF)
	}
	m := copy(p, n.data[int(n.pos):])
	n.pos += int64(m)
	if n.pos >= int64(len(n.data)) {
		// The final chunk of data is delivered together with the wrapped EOF.
		return m, fmt.Errorf("network stream closed: %w", io.EOF)
	}
	return m, nil
}

func (n *networkReadSeeker) Seek(offset int64, whence int) (int64, error) {
	var np int64
	switch whence {
	case io.SeekStart:
		np = offset
	case io.SeekCurrent:
		np = n.pos + offset
	case io.SeekEnd:
		np = int64(len(n.data)) + offset
	}
	if np < 0 {
		return 0, errors.New("negative position")
	}
	n.pos = np
	return np, nil
}

var _ io.ReadSeeker = (*networkReadSeeker)(nil)

// memWriteSeeker is an in-memory io.WriteSeeker supporting absolute seeks,
// used to build wav files with the Encoder.
type memWriteSeeker struct {
	buf []byte
	pos int64
}

func (w *memWriteSeeker) Write(p []byte) (int, error) {
	end := w.pos + int64(len(p))
	if end > int64(len(w.buf)) {
		w.buf = append(w.buf, make([]byte, end-int64(len(w.buf)))...)
	}
	copy(w.buf[w.pos:end], p)
	w.pos = end
	return len(p), nil
}

func (w *memWriteSeeker) Seek(offset int64, whence int) (int64, error) {
	var np int64
	switch whence {
	case io.SeekStart:
		np = offset
	case io.SeekCurrent:
		np = w.pos + offset
	case io.SeekEnd:
		np = int64(len(w.buf)) + offset
	}
	if np < 0 {
		return 0, errors.New("negative position")
	}
	w.pos = np
	return np, nil
}

// buildWAV encodes a mono 16-bit 8kHz wav containing numSamples samples,
// optionally with a LIST/INFO metadata chunk.
func buildWAV(t *testing.T, numSamples int, md *Metadata) []byte {
	t.Helper()
	ws := &memWriteSeeker{}
	e := NewEncoder(ws, 8000, 16, 1, 1)
	if md != nil {
		e.Metadata = md
	}
	ib := &audio.IntBuffer{
		Format: &audio.Format{NumChannels: 1, SampleRate: 8000},
		Data:   make([]int, numSamples),
	}
	for i := range ib.Data {
		ib.Data[i] = i
	}
	if err := e.Write(ib); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("close encoder: %v", err)
	}
	return ws.buf
}

// referencePCM decodes the PCM samples of a wav using a plain bytes.Reader
// (whose EOF is never wrapped), giving the expected full sample set.
func referencePCM(t *testing.T, data []byte) []int {
	t.Helper()
	d := NewDecoder(bytes.NewReader(data))
	buf, err := d.FullPCMBuffer()
	if err != nil {
		t.Fatalf("reference decode: %v", err)
	}
	return buf.Data
}

// TestWrappedEOF_ReadMetadata verifies that a normal end of file reported as
// a wrapped io.EOF is treated as success on the metadata path.
func TestWrappedEOF_ReadMetadata(t *testing.T) {
	// A real, well-formed file with a LIST/INFO chunk (the same fixture used
	// by the existing metadata tests) is used to assert decoded fields.
	fixture, err := os.ReadFile(filepath.Join("fixtures", "listinfo.wav"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	want := &Metadata{
		Artist: "artist", Title: "track title", Product: "album title",
		TrackNbr: "42", CreationDate: "2017", Genre: "genre", Comments: "my comment",
	}

	t.Run("fixture fields", func(t *testing.T) {
		d := NewDecoder(&networkReadSeeker{data: fixture})
		d.ReadMetadata()
		if err := d.Err(); err != nil {
			t.Fatalf("expected no error on clean end, got %v", err)
		}
		if !d.EOF() {
			t.Fatal("expected EOF() to be true after reading the whole container")
		}
		if d.Metadata == nil {
			t.Fatal("expected metadata to be decoded")
		}
		if !reflect.DeepEqual(d.Metadata, want) {
			t.Fatalf("metadata mismatch:\n got %#v\nwant %#v", d.Metadata, want)
		}
	})

	// Repeated status queries must be idempotent.
	d := NewDecoder(&networkReadSeeker{data: fixture})
	d.ReadMetadata()
	for i := 0; i < 3; i++ {
		if d.Err() != nil || !d.EOF() {
			t.Fatal("repeated Err/EOF queries changed the result")
		}
	}
}

// TestWrappedEOF_FullPCMBuffer verifies the full PCM path returns every
// sample and success when the reader wraps the normal end of file.
func TestWrappedEOF_FullPCMBuffer(t *testing.T) {
	data := buildWAV(t, 100, nil)
	want := referencePCM(t, data)

	d := NewDecoder(&networkReadSeeker{data: data})
	buf, err := d.FullPCMBuffer()
	if err != nil {
		t.Fatalf("expected clean end, got error %v", err)
	}
	if len(buf.Data) != len(want) {
		t.Fatalf("expected %d samples, got %d", len(want), len(buf.Data))
	}
	for i, v := range want {
		if buf.Data[i] != v {
			t.Fatalf("sample %d: expected %d, got %d", i, v, buf.Data[i])
		}
	}
	if d.Err() != nil {
		t.Fatalf("expected Err() nil, got %v", d.Err())
	}
	if !d.EOF() {
		t.Fatal("expected EOF() true after full PCM read")
	}
}

// TestWrappedEOF_PCMBufferSegments verifies the segmented PCM path: every
// sample is recovered across many small buffers with a clean end.
func TestWrappedEOF_PCMBufferSegments(t *testing.T) {
	data := buildWAV(t, 100, nil)
	want := referencePCM(t, data)

	d := NewDecoder(&networkReadSeeker{data: data})
	seg := &audio.IntBuffer{Data: make([]int, 7)}
	var got []int
	for {
		n, err := d.PCMBuffer(seg)
		if err != nil {
			t.Fatalf("unexpected error while segmenting: %v", err)
		}
		if n == 0 {
			break
		}
		got = append(got, seg.Data[:n]...)
	}
	if len(got) != len(want) {
		t.Fatalf("expected %d samples, got %d", len(want), len(got))
	}
	for i, v := range want {
		if got[i] != v {
			t.Fatalf("sample %d: expected %d, got %d", i, v, got[i])
		}
	}
	if d.Err() != nil {
		t.Fatalf("expected Err() nil, got %v", d.Err())
	}
	if !d.EOF() {
		t.Fatal("expected EOF() true after segmented PCM read")
	}
}

// TestWrappedEOF_NonEOFError verifies that a wrapped, non-EOF failure is not
// normalised away and stays traceable via errors.Is.
func TestWrappedEOF_NonEOFError(t *testing.T) {
	data := buildWAV(t, 100, nil)

	// Fail in the middle of the PCM data on every read path.
	t.Run("FullPCMBuffer", func(t *testing.T) {
		nr := &networkReadSeeker{
			data:    data,
			failAt:  int64(len(data) - 40),
			failErr: fmt.Errorf("connection reset: %w", errNetworkRead),
		}
		d := NewDecoder(nr)
		_, err := d.FullPCMBuffer()
		if err == nil {
			t.Fatal("expected an error, got nil")
		}
		if !errors.Is(err, errNetworkRead) {
			t.Fatalf("expected error to wrap errNetworkRead, got %v", err)
		}
		if !errors.Is(d.Err(), errNetworkRead) {
			t.Fatalf("expected Err() to keep the original failure, got %v", d.Err())
		}
		if d.EOF() {
			t.Fatal("a genuine I/O failure must not be reported as EOF")
		}
	})

	t.Run("PCMBuffer", func(t *testing.T) {
		nr := &networkReadSeeker{
			data:    data,
			failAt:  int64(len(data) - 40),
			failErr: fmt.Errorf("connection reset: %w", errNetworkRead),
		}
		d := NewDecoder(nr)
		seg := &audio.IntBuffer{Data: make([]int, 7)}
		var sawErr error
		for {
			n, err := d.PCMBuffer(seg)
			if err != nil {
				sawErr = err
				break
			}
			if n == 0 {
				break
			}
		}
		if sawErr == nil {
			t.Fatal("expected an error, got nil")
		}
		if !errors.Is(sawErr, errNetworkRead) {
			t.Fatalf("expected error to wrap errNetworkRead, got %v", sawErr)
		}
		if !errors.Is(d.Err(), errNetworkRead) {
			t.Fatalf("expected Err() to keep the original failure, got %v", d.Err())
		}
		if d.EOF() {
			t.Fatal("a genuine I/O failure must not be reported as EOF")
		}
	})

	t.Run("ReadMetadata", func(t *testing.T) {
		nr := &networkReadSeeker{
			data:    data,
			failAt:  int64(len(data) - 10),
			failErr: fmt.Errorf("connection reset: %w", errNetworkRead),
		}
		d := NewDecoder(nr)
		d.ReadMetadata()
		if d.Err() == nil {
			t.Fatal("expected an error, got nil")
		}
		if !errors.Is(d.Err(), errNetworkRead) {
			t.Fatalf("expected Err() to keep the original failure, got %v", d.Err())
		}
		if d.EOF() {
			t.Fatal("a genuine I/O failure must not be reported as EOF")
		}
	})
}

// TestWrappedEOF_EmptyInput verifies that empty input is reported rather than
// swallowed, on both a bare and a wrapped EOF reader.
func TestWrappedEOF_EmptyInput(t *testing.T) {
	for name, wrap := range map[string]bool{"bare EOF": false, "wrapped EOF": true} {
		t.Run(name, func(t *testing.T) {
			var r io.ReadSeeker
			if wrap {
				r = &networkReadSeeker{data: nil}
			} else {
				r = bytes.NewReader(nil)
			}
			d := NewDecoder(r)
			d.ReadInfo()
			if d.Err() == nil {
				t.Fatal("empty input must produce an error, got nil")
			}
			if !errors.Is(d.Err(), io.ErrUnexpectedEOF) {
				t.Fatalf("expected io.ErrUnexpectedEOF, got %v", d.Err())
			}
			if d.IsValidFile() {
				t.Fatal("empty input must not be considered a valid file")
			}
			// Repeated queries are stable.
			if !errors.Is(d.Err(), io.ErrUnexpectedEOF) {
				t.Fatal("repeated Err() query changed the result")
			}
		})
	}
}

// TestWrappedEOF_Truncated verifies that a container cut short is never
// silently swallowed: partial PCM data may be returned, but the error must be
// io.ErrUnexpectedEOF on every read path, with a bare and a wrapped EOF.
func TestWrappedEOF_Truncated(t *testing.T) {
	data := buildWAV(t, 100, nil)
	cut := len(data) - 60 // cut inside the PCM data

	for name, wrap := range map[string]bool{"bare EOF": false, "wrapped EOF": true} {
		t.Run(name, func(t *testing.T) {
			newReader := func() io.ReadSeeker {
				if wrap {
					return &networkReadSeeker{data: data[:cut]}
				}
				return bytes.NewReader(data[:cut])
			}

			t.Run("FullPCMBuffer", func(t *testing.T) {
				d := NewDecoder(newReader())
				buf, err := d.FullPCMBuffer()
				if err == nil {
					t.Fatal("expected unexpected EOF, got nil")
				}
				if !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatalf("expected io.ErrUnexpectedEOF, got %v", err)
				}
				if len(buf.Data) == 0 {
					t.Fatal("expected the partial PCM data to still be returned")
				}
				if !errors.Is(d.Err(), io.ErrUnexpectedEOF) {
					t.Fatalf("expected decoder Err() io.ErrUnexpectedEOF, got %v", d.Err())
				}
				if d.EOF() {
					t.Fatal("a truncated stream must not be reported as a clean EOF")
				}
			})

			t.Run("PCMBuffer", func(t *testing.T) {
				d := NewDecoder(newReader())
				seg := &audio.IntBuffer{Data: make([]int, 7)}
				var total int
				var sawErr error
				for {
					n, err := d.PCMBuffer(seg)
					total += n
					if err != nil {
						sawErr = err
						break
					}
					if n == 0 {
						break
					}
				}
				if sawErr == nil {
					t.Fatal("expected unexpected EOF, got nil")
				}
				if !errors.Is(sawErr, io.ErrUnexpectedEOF) {
					t.Fatalf("expected io.ErrUnexpectedEOF, got %v", sawErr)
				}
				if total == 0 {
					t.Fatal("expected partial samples before the truncation error")
				}
				if !errors.Is(d.Err(), io.ErrUnexpectedEOF) {
					t.Fatalf("expected decoder Err() io.ErrUnexpectedEOF, got %v", d.Err())
				}
				if d.EOF() {
					t.Fatal("a truncated stream must not be reported as a clean EOF")
				}
			})

			t.Run("ReadMetadata", func(t *testing.T) {
				d := NewDecoder(newReader())
				d.ReadMetadata()
				if d.Err() == nil {
					t.Fatal("expected an error on truncated input, got nil")
				}
				if !errors.Is(d.Err(), io.ErrUnexpectedEOF) {
					t.Fatalf("expected io.ErrUnexpectedEOF, got %v", d.Err())
				}
				if d.EOF() {
					t.Fatal("a truncated stream must not be reported as a clean EOF")
				}
			})
		})
	}
}

// TestWrappedEOF_TruncatedMetadataChunk verifies that a LIST chunk cut short
// is reported as io.ErrUnexpectedEOF rather than being silently accepted.
func TestWrappedEOF_TruncatedMetadataChunk(t *testing.T) {
	data := buildWAV(t, 100, &Metadata{Artist: "artist", Title: "track title"})
	// truncate inside the trailing LIST chunk (well after the PCM data)
	cut := len(data) - 8

	for name, wrap := range map[string]bool{"bare EOF": false, "wrapped EOF": true} {
		t.Run(name, func(t *testing.T) {
			var r io.ReadSeeker
			if wrap {
				r = &networkReadSeeker{data: data[:cut]}
			} else {
				r = bytes.NewReader(data[:cut])
			}
			d := NewDecoder(r)
			d.ReadMetadata()
			if d.Err() == nil {
				t.Fatal("expected an error for the truncated LIST chunk, got nil")
			}
			if !errors.Is(d.Err(), io.ErrUnexpectedEOF) {
				t.Fatalf("expected io.ErrUnexpectedEOF, got %v", d.Err())
			}
			if d.EOF() {
				t.Fatal("a truncated metadata chunk must not be reported as clean EOF")
			}
		})
	}
}

// TestPCMBuffer_LinearReadCost ensures that decoding through many tiny
// buffers keeps the underlying read work linear in the amount of data: each
// buffer must cost a small, bounded number of underlying reads instead of
// re-reading previously consumed bytes.
func TestPCMBuffer_LinearReadCost(t *testing.T) {
	const numSamples = 6000
	data := buildWAV(t, numSamples, nil)
	want := referencePCM(t, data)

	nr := &networkReadSeeker{data: data}
	d := NewDecoder(nr)
	seg := &audio.IntBuffer{Data: make([]int, 1)}
	var got []int
	for {
		n, err := d.PCMBuffer(seg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if n == 0 {
			break
		}
		got = append(got, seg.Data[:n]...)
	}
	if len(got) != len(want) {
		t.Fatalf("expected %d samples, got %d", len(want), len(got))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sample %d mismatch: expected %d, got %d", i, want[i], got[i])
		}
	}
	// One bounded read per tiny buffer plus a handful of header reads keeps
	// the total work linear (a few reads per sample at most, never quadratic).
	if nr.readCalls > numSamples*3+16 {
		t.Fatalf("read work looks super-linear: %d reads for %d samples",
			nr.readCalls, numSamples)
	}
	if !d.EOF() || d.Err() != nil {
		t.Fatalf("expected clean EOF, got EOF=%v Err=%v", d.EOF(), d.Err())
	}
}
