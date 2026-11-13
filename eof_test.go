package wav

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"reflect"
	"testing"

	"github.com/go-audio/audio"
)

// wrappedEOFReader wraps the underlying reader so that every terminal error,
// including io.EOF, is returned inside another error instead of as the bare
// io.EOF sentinel. This mimics network buffered readers which decorate the
// errors they surface.
type wrappedEOFReader struct {
	r        io.ReadSeeker
	readCnt  int
	failWith error
	failAt   int // when > 0, failWith is returned from the failAt-th Read on
	failed   bool
}

func (w *wrappedEOFReader) Read(p []byte) (int, error) {
	w.readCnt++
	if w.failAt > 0 && (w.failed || w.readCnt >= w.failAt) {
		w.failed = true
		// A transport failure is sticky: every further read keeps failing.
		return 0, fmt.Errorf("wrapped read failure: %w", w.failWith)
	}
	n, err := w.r.Read(p)
	if err != nil {
		return n, fmt.Errorf("wrapped reader: %w", err)
	}
	return n, nil
}

func (w *wrappedEOFReader) Seek(offset int64, whence int) (int64, error) {
	return w.r.Seek(offset, whence)
}

// errReadSeeker is a ReadSeeker which always returns the same non-EOF error.
type errReadSeeker struct {
	err error
}

func (e *errReadSeeker) Read(p []byte) (int, error) {
	return 0, e.err
}

func (e *errReadSeeker) Seek(offset int64, whence int) (int64, error) {
	return 0, nil
}

// truncatingReadSeeker serves at most limit bytes of the wrapped data, then
// behaves like an exhausted reader. With hard set, further reads fail with
// io.ErrUnexpectedEOF instead of the clean io.EOF sentinel.
type truncatingReadSeeker struct {
	data  []byte
	pos   int64
	limit int64
	hard  bool
}

func (t *truncatingReadSeeker) Read(p []byte) (int, error) {
	if t.pos >= t.limit {
		if t.hard {
			return 0, io.ErrUnexpectedEOF
		}
		return 0, io.EOF
	}
	n := copy(p, t.data[t.pos:])
	if int64(n) > t.limit-t.pos {
		n = int(t.limit - t.pos)
	}
	t.pos += int64(n)
	if t.pos >= t.limit {
		if t.hard {
			return n, io.ErrUnexpectedEOF
		}
		return n, io.EOF
	}
	return n, nil
}

func (t *truncatingReadSeeker) Seek(offset int64, whence int) (int64, error) {
	var newPos int64
	switch whence {
	case io.SeekStart:
		newPos = offset
	case io.SeekCurrent:
		newPos = t.pos + offset
	case io.SeekEnd:
		newPos = int64(len(t.data)) + offset
	}
	if newPos < 0 {
		return 0, errors.New("negative position")
	}
	t.pos = newPos
	return newPos, nil
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := ioutil.ReadFile(path)
	if err != nil {
		t.Fatalf("couldn't read %s: %v", path, err)
	}
	return data
}

// baselinePCM decodes the complete PCM buffer using a plain bytes.Reader so
// the wrapped-reader results can be compared against an untouched reference.
func baselinePCM(t *testing.T, data []byte) []int {
	t.Helper()
	d := NewDecoder(bytes.NewReader(data))
	buf, err := d.FullPCMBuffer()
	if err != nil {
		t.Fatalf("baseline FullPCMBuffer failed: %v", err)
	}
	return buf.Data
}

// TestDecoder_WrappedEOF_Metadata makes sure a reader which never returns the
// bare io.EOF but only wrapped EOFs does not surface the normal end of file as
// a business error on the metadata path.
func TestDecoder_WrappedEOF_Metadata(t *testing.T) {
	data := mustReadFile(t, "fixtures/listinfo.wav")
	wantMeta := &Metadata{
		Artist: "artist", Title: "track title", Product: "album title",
		TrackNbr: "42", CreationDate: "2017", Genre: "genre", Comments: "my comment",
	}

	wr := &wrappedEOFReader{r: bytes.NewReader(data)}
	d := NewDecoder(wr)
	d.ReadMetadata()

	if err := d.Err(); err != nil {
		t.Fatalf("wrapped EOF should not be reported as an error, got: %v", err)
	}
	if !d.EOF() {
		t.Fatal("EOF() should be true after the full file was read through a wrapping reader")
	}
	// repeated status queries must be stable
	if d.Err() != nil || !d.EOF() {
		t.Fatal("repeated Err/EOF queries changed the result")
	}
	if d.Metadata == nil {
		t.Fatal("expected metadata to be decoded")
	}
	if !reflect.DeepEqual(d.Metadata, wantMeta) {
		t.Fatalf("metadata mismatch\nwant: %#v\ngot:  %#v", wantMeta, d.Metadata)
	}
}

// TestDecoder_WrappedEOF_FullPCMBuffer covers the full in-memory PCM path.
func TestDecoder_WrappedEOF_FullPCMBuffer(t *testing.T) {
	data := mustReadFile(t, "fixtures/kick.wav")
	want := baselinePCM(t, data)

	wr := &wrappedEOFReader{r: bytes.NewReader(data)}
	d := NewDecoder(wr)
	buf, err := d.FullPCMBuffer()
	if err != nil {
		t.Fatalf("FullPCMBuffer returned a wrapped EOF as error: %v", err)
	}
	if !reflect.DeepEqual(buf.Data, want) {
		t.Fatalf("PCM data mismatch: got %d samples, want %d", len(buf.Data), len(want))
	}
	if err := d.Err(); err != nil {
		t.Fatalf("Err() should be nil at normal end, got: %v", err)
	}
	if !d.EOF() {
		t.Fatal("EOF() should be true at normal end")
	}
	if d.Err() != nil || !d.EOF() {
		t.Fatal("repeated Err/EOF queries changed the result")
	}
}

// TestDecoder_WrappedEOF_PCMBuffer covers the chunked PCM path and checks that
// every sample is recovered and the terminal read is reported as success.
func TestDecoder_WrappedEOF_PCMBuffer(t *testing.T) {
	data := mustReadFile(t, "fixtures/kick.wav")
	want := baselinePCM(t, data)

	wr := &wrappedEOFReader{r: bytes.NewReader(data)}
	d := NewDecoder(wr)

	got := make([]int, 0, len(want))
	intBuf := make([]int, 128)
	buf := &audio.IntBuffer{Data: intBuf}
	var total int
	for {
		n, err := d.PCMBuffer(buf)
		if err != nil {
			t.Fatalf("PCMBuffer returned wrapped EOF as error: %v", err)
		}
		if n == 0 {
			break
		}
		total += n
		got = append(got, buf.Data[:n]...)
	}
	if total != len(want) {
		t.Fatalf("expected %d samples, got %d", len(want), total)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("chunked PCM data differs from the baseline decode")
	}
	if err := d.Err(); err != nil {
		t.Fatalf("Err() should be nil at normal end, got: %v", err)
	}
	if !d.EOF() {
		t.Fatal("EOF() should be true at normal end")
	}
	if d.Err() != nil || !d.EOF() {
		t.Fatal("repeated Err/EOF queries changed the result")
	}
}

// TestDecoder_WrappedNonEOFErrorPreserved checks that a wrapped error which is
// NOT an end of file stays fully traceable through errors.Is/As on every path.
func TestDecoder_WrappedNonEOFErrorPreserved(t *testing.T) {
	sentinel := errors.New("simulated network reset")

	t.Run("metadata path", func(t *testing.T) {
		data := mustReadFile(t, "fixtures/listinfo.wav")
		wr := &wrappedEOFReader{r: bytes.NewReader(data), failWith: sentinel, failAt: 2}
		d := NewDecoder(wr)
		d.ReadMetadata()
		if err := d.Err(); err == nil {
			t.Fatal("expected the non-EOF error to be surfaced")
		} else if !errors.Is(err, sentinel) {
			t.Fatalf("error chain lost the original error, got: %v", err)
		}
	})

	t.Run("full PCM path", func(t *testing.T) {
		data := mustReadFile(t, "fixtures/kick.wav")
		wr := &wrappedEOFReader{r: bytes.NewReader(data), failWith: sentinel, failAt: 8}
		d := NewDecoder(wr)
		_, err := d.FullPCMBuffer()
		if err == nil {
			t.Fatal("expected the non-EOF error to be surfaced")
		}
		if !errors.Is(err, sentinel) {
			t.Fatalf("error chain lost the original error, got: %v", err)
		}
		if !errors.Is(d.Err(), sentinel) {
			t.Fatalf("Err() lost the original error, got: %v", d.Err())
		}
	})

	t.Run("chunked PCM path", func(t *testing.T) {
		data := mustReadFile(t, "fixtures/kick.wav")
		wr := &wrappedEOFReader{r: bytes.NewReader(data), failWith: sentinel, failAt: 8}
		d := NewDecoder(wr)
		buf := &audio.IntBuffer{Data: make([]int, 128)}
		var sawErr error
		for {
			n, err := d.PCMBuffer(buf)
			if err != nil {
				sawErr = err
				break
			}
			if n == 0 {
				break
			}
		}
		if sawErr == nil {
			t.Fatal("expected the non-EOF error to be surfaced")
		}
		if !errors.Is(sawErr, sentinel) {
			t.Fatalf("error chain lost the original error, got: %v", sawErr)
		}
	})

	t.Run("header read failure", func(t *testing.T) {
		d := NewDecoder(&errReadSeeker{err: fmt.Errorf("connection lost: %w", sentinel)})
		d.ReadInfo()
		if err := d.Err(); err == nil {
			t.Fatal("expected the non-EOF error to be surfaced")
		} else if !errors.Is(err, sentinel) {
			t.Fatalf("error chain lost the original error, got: %v", err)
		}
	})
}

// TestDecoder_WrappedEOF_EmptyInput verifies that empty input is not swallowed
// as a successful decode: no headers can be parsed and the EOF state is visible.
func TestDecoder_WrappedEOF_EmptyInput(t *testing.T) {
	wr := &wrappedEOFReader{r: bytes.NewReader(nil)}
	d := NewDecoder(wr)
	d.ReadInfo()
	// The wrapped EOF is the terminal state: Err() hides the clean EOF by
	// contract, EOF() must surface it and the file cannot be valid.
	if d.Err() != nil {
		t.Fatalf("clean EOF should not be reported as a business error, got: %v", d.Err())
	}
	if !d.EOF() {
		t.Fatal("empty input should be reported as EOF")
	}
	if d.IsValidFile() {
		t.Fatal("empty input must not be considered a valid wav file")
	}

	wr2 := &wrappedEOFReader{r: bytes.NewReader(nil)}
	d2 := NewDecoder(wr2)
	if err := d2.FwdToPCM(); err == nil {
		t.Fatal("empty input must fail to reach a PCM chunk")
	}
	if _, err := d2.FullPCMBuffer(); err == nil {
		t.Fatal("empty input must fail to produce a PCM buffer")
	}
	if !d2.EOF() {
		t.Fatal("empty input should leave the decoder in EOF state")
	}
}

// TestDecoder_WrappedEOF_TruncatedInput verifies that a file cut short in the
// middle of its PCM chunk is not silently treated as a complete file: the
// decoded sample count falls short and the early end is recorded.
func TestDecoder_WrappedEOF_TruncatedInput(t *testing.T) {
	data := mustReadFile(t, "fixtures/kick.wav")
	want := baselinePCM(t, data)

	// cut the file in half: the RIFF/data headers still promise the full size.
	trunc := &truncatingReadSeeker{data: data, limit: int64(len(data) / 2)}
	wr := &wrappedEOFReader{r: trunc}
	d := NewDecoder(wr)
	buf, err := d.FullPCMBuffer()
	if err != nil {
		t.Fatalf("truncation should surface through EOF state, got: %v", err)
	}
	if len(buf.Data) >= len(want) {
		t.Fatalf("truncated input returned %d samples, expected fewer than the %d promised samples", len(buf.Data), len(want))
	}
	// the decoder must remember the early end of the promised stream
	if !d.EOF() {
		t.Fatalf("truncated input should record the EOF, got eof=%t", d.EOF())
	}

	// same scenario through the chunked path
	trunc2 := &truncatingReadSeeker{data: data, limit: int64(len(data) / 2)}
	wr2 := &wrappedEOFReader{r: trunc2}
	d2 := NewDecoder(wr2)
	pcmBuf := &audio.IntBuffer{Data: make([]int, 128)}
	var total int
	for {
		n, err := d2.PCMBuffer(pcmBuf)
		if err != nil {
			t.Fatalf("PCMBuffer should not return the EOF itself: %v", err)
		}
		if n == 0 {
			break
		}
		total += n
	}
	if total >= len(want) {
		t.Fatalf("truncated chunked read returned %d samples, expected fewer than %d", total, len(want))
	}
	if !d2.EOF() {
		t.Fatalf("truncated chunked read should record the EOF, got eof=%t", d2.EOF())
	}
}

// TestDecoder_UnexpectedEOF_NotSwallowed checks the hard truncation case where
// a read fails mid-chunk with io.ErrUnexpectedEOF. Unlike a clean EOF this
// must remain visible through Err() and must not be swallowed as success.
func TestDecoder_UnexpectedEOF_NotSwallowed(t *testing.T) {
	data := mustReadFile(t, "fixtures/kick.wav")
	want := baselinePCM(t, data)

	trunc := &truncatingReadSeeker{data: data, limit: int64(len(data) / 2), hard: true}
	wr := &wrappedEOFReader{r: trunc}
	d := NewDecoder(wr)
	buf, _ := d.FullPCMBuffer()
	if len(buf.Data) >= len(want) {
		t.Fatalf("hard-truncated input returned %d samples, expected fewer than %d", len(buf.Data), len(want))
	}
	if err := d.Err(); err == nil {
		t.Fatal("io.ErrUnexpectedEOF must be surfaced through Err(), not swallowed")
	} else if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("expected io.ErrUnexpectedEOF in the error chain, got: %v", err)
	}
}

// TestDecoder_WrappedEOF_StatusIdempotent repeatedly queries the status APIs
// while decoding to make sure they never mutate the stored state.
func TestDecoder_WrappedEOF_StatusIdempotent(t *testing.T) {
	data := mustReadFile(t, "fixtures/bass.wav")
	wr := &wrappedEOFReader{r: bytes.NewReader(data)}
	d := NewDecoder(wr)

	buf := &audio.IntBuffer{Data: make([]int, 64)}
	var total int
	for {
		n, err := d.PCMBuffer(buf)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// querying state in the middle of the stream must not change anything
		eofBefore := d.EOF()
		errBefore := d.Err()
		for i := 0; i < 5; i++ {
			if d.EOF() != eofBefore || !reflect.DeepEqual(d.Err(), errBefore) {
				t.Fatal("repeated Err/EOF queries changed the result mid-stream")
			}
		}
		if n == 0 {
			break
		}
		total += n
	}
	if total == 0 {
		t.Fatal("expected to decode samples")
	}
	if d.Err() != nil || !d.EOF() {
		t.Fatalf("expected clean end state, got err=%v eof=%t", d.Err(), d.EOF())
	}
}

// TestDecoder_WrappedEOF_LinearReadCost feeds a wav file through a reader
// returning one byte per call and checks that processing 6000+ small reads
// keeps the number of reads linear in the amount of data.
func TestDecoder_WrappedEOF_LinearReadCost(t *testing.T) {
	data := mustReadFile(t, "fixtures/8bit.wav")
	want := baselinePCM(t, data)

	wr := &oneByteAtATimeReader{r: bytes.NewReader(data), max: 1}
	d := NewDecoder(wr)
	buf := &audio.IntBuffer{Data: make([]int, 256)}
	var samples int
	for {
		n, err := d.PCMBuffer(buf)
		if err != nil {
			t.Fatalf("unexpected error during slow read: %v", err)
		}
		if n == 0 {
			break
		}
		samples += n
	}
	if err := d.Err(); err != nil {
		t.Fatalf("unexpected decoder error: %v", err)
	}
	if samples != len(want) {
		t.Fatalf("expected %d samples, got %d", len(want), samples)
	}
	if wr.readCnt < 6000 {
		t.Fatalf("expected at least 6000 small reads, got %d", wr.readCnt)
	}
	// Every byte must be touched at most a small constant number of times:
	// read count linear in file size, never quadratic.
	if wr.readCnt > len(data)*4 {
		t.Fatalf("read count %d is not linear in file size %d", wr.readCnt, len(data))
	}
}

// oneByteAtATimeReader returns at most max bytes per Read and wraps io.EOF so
// the decoder cannot rely on the bare sentinel.
type oneByteAtATimeReader struct {
	r       io.ReadSeeker
	max     int
	readCnt int
}

func (o *oneByteAtATimeReader) Read(p []byte) (int, error) {
	o.readCnt++
	if o.max > 0 && len(p) > o.max {
		p = p[:o.max]
	}
	n, err := o.r.Read(p)
	if err != nil {
		return n, fmt.Errorf("slow reader: %w", err)
	}
	return n, nil
}

func (o *oneByteAtATimeReader) Seek(offset int64, whence int) (int64, error) {
	return o.r.Seek(offset, whence)
}
