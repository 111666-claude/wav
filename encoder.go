package wav

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/go-audio/audio"
	"github.com/go-audio/riff"
)

const (
	// maxUint32Value is the largest value a 32-bit RIFF size field can hold.
	maxUint32Value = uint64(1)<<32 - 1
	// maxUint16Value is the largest value a 16-bit RIFF field can hold.
	maxUint16Value = uint64(1)<<16 - 1
	// maxInt64Value is the largest value an int64 can hold.
	maxInt64Value = int64(^uint64(0) >> 1)

	// ds64ChunkSize is the size of a ds64 chunk payload without the optional
	// table of additional chunk sizes. It holds riffSize, dataSize and
	// sampleCount (3 * 8 bytes) followed by a 32-bit table length.
	ds64ChunkSize = 28

	// placeholder32 is the value used for 32-bit size fields whose real value
	// is either unknown at header-write time (RIFF) or stored in the ds64
	// chunk (RF64).
	placeholder32 = uint32(0xFFFFFFFF)

	// rf64RiffSizePos, rf64DataSizePos and rf64SampleCountPos are the byte
	// offsets of the 64-bit fields of the first ds64 chunk, relative to the
	// start of the container.
	rf64RiffSizePos    = int64(20)
	rf64DataSizePos    = int64(28)
	rf64SampleCountPos = int64(36)
)

var (
	// ErrUnseekable is returned when the underlying writer can't seek and the
	// encoder therefore can't backfill the container sizes on Close.
	ErrUnseekable = errors.New("wav: writer is not seekable")

	// ErrSizeOverflow is returned when the encoded sizes don't fit in the
	// selected container. Classic RIFF files are limited to 4 GiB; RF64 files
	// are limited to the largest sizes representable by a signed 64-bit
	// counter.
	ErrSizeOverflow = errors.New("wav: encoded size exceeds the container limit")

	// ErrClosed is returned when writing to an encoder that was already
	// closed.
	ErrClosed = errors.New("wav: encoder is closed")

	// ErrNoData is returned by Close when no header was ever written, for
	// instance when Close is called without a single call to Write or
	// WriteFrame. Calling Close in that state can't produce a valid file.
	ErrNoData = errors.New("wav: no audio data was written")

	// ErrModeLocked is returned when RF64 is enabled or disabled after the
	// header was written. The container mode is fixed once the header is out.
	ErrModeLocked = errors.New("wav: container mode can't change after writing started")
)

var (
	// rf64ID is the form type used by the RF64 (BW64) container.
	rf64ID = [4]byte{'R', 'F', '6', '4'}
	// ds64ID is the ID of the mandatory first chunk of an RF64 file.
	ds64ID = [4]byte{'d', 's', '6', '4'}
)

// initialBufferCapacity returns a safe initial capacity for the staging
// buffer, avoiding a division by zero or an absurd allocation for invalid or
// extreme encoder configurations.
func initialBufferCapacity(sampleRate, bitDepth, numChans int) int {
	const maxHint = 1 << 20 // 1 MiB is plenty to batch writes
	// time.Second/sampleRate truncates to zero above 1 GHz, so reject values
	// the header validation rejects anyway before calling the duration helper.
	if sampleRate <= 0 || sampleRate > 1e9 || bitDepth < 8 || numChans <= 0 {
		return 0
	}
	bytes := int64(bytesNumFromDuration(time.Minute, sampleRate, bitDepth)) * int64(numChans)
	if bytes <= 0 || bytes > maxHint {
		return maxHint
	}
	return int(bytes)
}

// EncoderOption configures an Encoder when it is created.
type EncoderOption func(*Encoder)

// WithRF64 selects the RF64 (BW64) container instead of classic RIFF. Small
// files are allowed too. This is equivalent to setting Encoder.RF64 before the
// first write, but makes the intent explicit at construction time.
func WithRF64() EncoderOption {
	return func(e *Encoder) { e.RF64 = true }
}

// Encoder encodes LPCM data into a wav container.
type Encoder struct {
	w   io.WriteSeeker
	buf *bytes.Buffer

	SampleRate int
	BitDepth   int
	NumChans   int

	// A number indicating the WAVE format category of the file. The content of
	// the <format-specific-fields> portion of the ‘fmt’ chunk, and the
	// interpretation of the waveform data, depend on this value. PCM = 1 (i.e.
	// Linear quantization) Values other than 1 indicate some form of
	// compression.
	WavAudioFormat int

	// Metadata contains metadata to inject in the file.
	Metadata *Metadata

	// RF64 selects the RF64 (BW64) container instead of classic RIFF. It must
	// be set before the first call to Write or WriteFrame; afterwards the
	// container mode is locked and ErrModeLocked is returned. RF64 files can
	// hold more than 4 GiB of PCM data and store the real file, data and
	// sample-frame counts in the mandatory ds64 chunk. Small files can use
	// RF64 as well. Use NewRF64Encoder, WithRF64 or SetRF64 to opt in.
	RF64 bool

	// WrittenBytes is the number of bytes physically written to the underlying
	// writer so far. Container size arithmetic uses 64-bit counters; this
	// field is kept for compatibility.
	WrittenBytes int

	// frames is the number of sample frames (samples per channel) written.
	frames int64

	writtenBytes int64
	// baseOffset is the writer offset at which this container starts.
	baseOffset int64
	// pcmDataStart is the writer offset of the first PCM byte.
	pcmDataStart    int64
	pcmChunkStarted bool
	pcmChunkSizePos int64

	wroteHeader bool // true if we've written (or deliberately skipped) the header

	// rf64 is the container mode snapshot taken when the header was written.
	rf64 bool

	// err is the first write-path error. It is sticky: once a write failed the
	// encoder refuses to report the resulting partial file as a success.
	err error

	// closeErr is the result of the first Close call. Close is idempotent and
	// returns the same result on repeated calls.
	closeErr error
	closed   bool
}

// NewEncoder creates a new encoder to create a new wav file.
// Don't forget to add Frames to the encoder before writing.
// The default container is classic RIFF. Pass WithRF64 (or use
// NewRF64Encoder, or set RF64 before the first write) to explicitly write an
// RF64 container.
func NewEncoder(w io.WriteSeeker, sampleRate, bitDepth, numChans, audioFormat int, opts ...EncoderOption) *Encoder {
	e := &Encoder{
		w:              w,
		buf:            bytes.NewBuffer(make([]byte, 0, initialBufferCapacity(sampleRate, bitDepth, numChans))),
		SampleRate:     sampleRate,
		BitDepth:       bitDepth,
		NumChans:       numChans,
		WavAudioFormat: audioFormat,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(e)
		}
	}
	return e
}

// NewRF64Encoder creates a new encoder that writes an RF64 (BW64) container.
// This is the explicit opt-in for files that may exceed the 4 GiB limit of
// classic RIFF. Small files are allowed as well.
func NewRF64Encoder(w io.WriteSeeker, sampleRate, bitDepth, numChans, audioFormat int) *Encoder {
	return NewEncoder(w, sampleRate, bitDepth, numChans, audioFormat, WithRF64())
}

// NewEncoderRF64 is an alias for NewRF64Encoder.
func NewEncoderRF64(w io.WriteSeeker, sampleRate, bitDepth, numChans, audioFormat int) *Encoder {
	return NewRF64Encoder(w, sampleRate, bitDepth, numChans, audioFormat)
}

// SetRF64 enables or disables explicit RF64 output. It returns ErrModeLocked
// if the header was already written.
func (e *Encoder) SetRF64(enabled bool) error {
	if e == nil {
		return errors.New("wav: can't configure a nil encoder")
	}
	if e.RF64 == enabled {
		return nil
	}
	if e.wroteHeader || e.writtenBytes > 0 {
		return ErrModeLocked
	}
	e.RF64 = enabled
	return nil
}

// AddLE serializes and adds the passed value using little endian
func (e *Encoder) AddLE(src interface{}) error {
	var b bytes.Buffer
	if err := binary.Write(&b, binary.LittleEndian, src); err != nil {
		return err
	}
	return e.write(b.Bytes())
}

// AddBE serializes and adds the passed value using big endian
func (e *Encoder) AddBE(src interface{}) error {
	var b bytes.Buffer
	if err := binary.Write(&b, binary.BigEndian, src); err != nil {
		return err
	}
	return e.write(b.Bytes())
}

// write writes p, records the actual number of bytes written and turns any
// error (including short writes) into a sticky encoder error.
func (e *Encoder) write(p []byte) error {
	if e.err != nil {
		return e.err
	}
	if len(p) == 0 {
		return nil
	}
	n, err := e.w.Write(p)
	if n > 0 {
		e.writtenBytes += int64(n)
		e.WrittenBytes = int(e.writtenBytes)
	}
	if err != nil {
		e.err = err
		return err
	}
	if n != len(p) {
		e.err = io.ErrShortWrite
		return e.err
	}
	return nil
}

// blockAlign returns the number of bytes in one sample frame, validating the
// encoder configuration so that the header fields can't silently overflow.
func (e *Encoder) blockAlign() (int, error) {
	switch e.BitDepth {
	case 8, 16, 24, 32:
	default:
		return 0, fmt.Errorf("wav: unsupported bit depth %d", e.BitDepth)
	}
	if e.NumChans <= 0 {
		return 0, fmt.Errorf("wav: invalid number of channels %d", e.NumChans)
	}
	if uint64(e.NumChans) > maxUint16Value {
		return 0, fmt.Errorf("wav: number of channels %d doesn't fit in a 16-bit field", e.NumChans)
	}
	align := int64(e.NumChans) * int64(e.BitDepth) / 8
	if align <= 0 || uint64(align) > maxUint16Value {
		return 0, fmt.Errorf("wav: block alignment %d doesn't fit in a 16-bit field", align)
	}
	return int(align), nil
}

// dataBytes returns the size of the PCM data described by the frame counter.
func (e *Encoder) dataBytes() (int64, error) {
	blockAlign, err := e.blockAlign()
	if err != nil {
		return 0, err
	}
	if e.frames < 0 {
		return 0, fmt.Errorf("%w: negative frame count", ErrSizeOverflow)
	}
	if e.frames > maxInt64Value/int64(blockAlign) {
		return 0, fmt.Errorf("%w: data size overflow", ErrSizeOverflow)
	}
	return e.frames * int64(blockAlign), nil
}

// checkFrameDelta verifies that adding n frames won't overflow the frame or
// byte counters and that the result fits in the selected container. It is
// called before any PCM data is written so that overflow is reported without
// producing a truncated file.
func (e *Encoder) checkFrameDelta(n int64) error {
	if n < 0 {
		return fmt.Errorf("%w: negative frame count", ErrSizeOverflow)
	}
	blockAlign, err := e.blockAlign()
	if err != nil {
		return err
	}
	if e.frames > maxInt64Value-n {
		return fmt.Errorf("%w: frame count overflow", ErrSizeOverflow)
	}
	nextFrames := e.frames + n
	if nextFrames > maxInt64Value/int64(blockAlign) {
		return fmt.Errorf("%w: data size overflow", ErrSizeOverflow)
	}
	if nextFrames*int64(blockAlign) > maxInt64Value-e.pcmDataStart {
		return fmt.Errorf("%w: container size overflow", ErrSizeOverflow)
	}
	if e.rf64 {
		return nil
	}

	dataBytes := nextFrames * int64(blockAlign)
	if dataBytes > int64(maxUint32Value) {
		return fmt.Errorf("%w: RIFF data chunk can't exceed 4 GiB, use RF64", ErrSizeOverflow)
	}
	// The RIFF size field covers everything after the 8 byte RIFF header.
	// pcmDataStart is relative to the start of the container, so the field is
	// (pcmDataStart-8)+dataBytes plus any trailer bytes written later
	// (padding, metadata), which Close checks again.
	riffSize := (e.pcmDataStart - 8) + dataBytes
	if riffSize > int64(maxUint32Value) {
		return fmt.Errorf("%w: RIFF file can't exceed 4 GiB, use RF64", ErrSizeOverflow)
	}
	return nil
}

// validateBuffer checks the buffer is compatible with the encoder
// configuration. Input validation errors are not sticky: the caller can fix
// the buffer and retry.
func (e *Encoder) validateBuffer(buf *audio.IntBuffer) error {
	if buf == nil {
		return fmt.Errorf("can't add a nil buffer")
	}
	if buf.Format == nil {
		return fmt.Errorf("can't add a buffer with a nil format")
	}
	if buf.Format.NumChannels <= 0 {
		return fmt.Errorf("wav: buffer has an invalid number of channels %d", buf.Format.NumChannels)
	}
	if buf.Format.NumChannels != e.NumChans {
		return fmt.Errorf("wav: buffer has %d channels, encoder was configured with %d", buf.Format.NumChannels, e.NumChans)
	}
	// Note: like the original implementation, trailing values that don't form
	// a whole frame are ignored (NumFrames floors the division).
	return nil
}

func (e *Encoder) addBuffer(buf *audio.IntBuffer) error {
	if err := e.validateBuffer(buf); err != nil {
		return err
	}

	frameCount := buf.NumFrames()
	if err := e.checkFrameDelta(int64(frameCount)); err != nil {
		return err
	}

	// performance tweak: setup a buffer so we don't do too many writes
	for i := 0; i < frameCount; i++ {
		for j := 0; j < buf.Format.NumChannels; j++ {
			v := buf.Data[i*buf.Format.NumChannels+j]
			switch e.BitDepth {
			case 8:
				if err := binary.Write(e.buf, binary.LittleEndian, uint8(v)); err != nil {
					return err
				}
			case 16:
				if err := binary.Write(e.buf, binary.LittleEndian, int16(v)); err != nil {
					return err
				}
			case 24:
				if err := binary.Write(e.buf, binary.LittleEndian, audio.Int32toInt24LEBytes(int32(v))); err != nil {
					return err
				}
			case 32:
				if err := binary.Write(e.buf, binary.LittleEndian, int32(v)); err != nil {
					return err
				}
			default:
				return fmt.Errorf("can't add frames of bit size %d", e.BitDepth)
			}
		}
	}

	if err := e.write(e.buf.Bytes()); err != nil {
		return err
	}
	e.frames += int64(frameCount)
	e.buf.Reset()

	return nil
}

func (e *Encoder) writeHeader() error {
	if e == nil {
		return fmt.Errorf("can't write a nil encoder")
	}
	if e.w == nil {
		return fmt.Errorf("can't write to a nil writer")
	}
	if e.wroteHeader {
		return errors.New("already wrote header")
	}
	if e.writtenBytes > 0 {
		// Someone wrote raw values with AddLE/AddBE before the first Write.
		// Preserve the historical behavior of not emitting a header. RF64
		// needs the ds64 chunk this path skips, so it can't be combined.
		if e.RF64 {
			return fmt.Errorf("wav: RF64 can't be used after raw bytes were written with AddLE/AddBE")
		}
		e.wroteHeader = true
		e.rf64 = false
		e.baseOffset = 0
		return nil
	}

	blockAlign, err := e.blockAlign()
	if err != nil {
		return err
	}
	if e.SampleRate <= 0 {
		return fmt.Errorf("wav: invalid sample rate %d", e.SampleRate)
	}
	if uint64(e.SampleRate) > maxUint32Value {
		return fmt.Errorf("wav: sample rate %d doesn't fit in a 32-bit field", e.SampleRate)
	}
	if uint64(e.SampleRate)*uint64(blockAlign) > maxUint32Value {
		return fmt.Errorf("wav: average bytes per second overflows a 32-bit field")
	}
	if e.WavAudioFormat < 0 || uint64(e.WavAudioFormat) > maxUint16Value {
		return fmt.Errorf("wav: audio format %d doesn't fit in a 16-bit field", e.WavAudioFormat)
	}

	// Sizes are backfilled on Close, so fail early and explicitly if the
	// writer can't seek instead of producing a file with unusable sizes.
	// SeekCurrent doesn't move the cursor, which lets the encoder be embedded
	// after data already present in the writer.
	off, err := e.w.Seek(0, io.SeekCurrent)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnseekable, err)
	}
	e.baseOffset = off

	e.rf64 = e.RF64

	if e.rf64 {
		// RF64 header. The 32-bit RIFF size and data chunk size are
		// placeholders; the real 64-bit values live in the ds64 chunk, which
		// must be the first chunk after the WAVE form type.
		if err := e.AddLE(rf64ID); err != nil { // form type
			return err
		}
		if err := e.AddLE(placeholder32); err != nil { // size, must be 0xFFFFFFFF
			return err
		}
		if err := e.AddLE(riff.WavFormatID); err != nil {
			return err
		}
		// ds64 chunk
		if err := e.AddLE(ds64ID); err != nil {
			return err
		}
		if err := e.AddLE(uint32(ds64ChunkSize)); err != nil {
			return err
		}
		// riffSize, dataSize and sampleCount are backfilled by Close.
		if err := e.AddLE(uint64(0)); err != nil {
			return err
		}
		if err := e.AddLE(uint64(0)); err != nil {
			return err
		}
		if err := e.AddLE(uint64(0)); err != nil {
			return err
		}
		// Number of additional chunk size entries in the ds64 table.
		if err := e.AddLE(uint32(0)); err != nil {
			return err
		}
	} else {
		// riff ID
		if err := e.AddLE(riff.RiffID); err != nil {
			return err
		}
		// file size uint32, to update later on.
		if err := e.AddLE(placeholder32); err != nil {
			return err
		}
		// wave headers
		if err := e.AddLE(riff.WavFormatID); err != nil {
			return err
		}
	}
	// form
	if err := e.AddLE(riff.FmtID); err != nil {
		return err
	}
	// chunk size
	if err := e.AddLE(uint32(16)); err != nil {
		return err
	}
	// wave format
	if err := e.AddLE(uint16(e.WavAudioFormat)); err != nil {
		return err
	}
	// num channels
	if err := e.AddLE(uint16(e.NumChans)); err != nil {
		return fmt.Errorf("error encoding the number of channels - %w", err)
	}
	// samplerate
	if err := e.AddLE(uint32(e.SampleRate)); err != nil {
		return fmt.Errorf("error encoding the sample rate - %w", err)
	}
	// avg bytes per sec
	if err := e.AddLE(uint32(uint64(e.SampleRate) * uint64(blockAlign))); err != nil {
		return fmt.Errorf("error encoding the avg bytes per sec - %w", err)
	}
	// block align
	if err := e.AddLE(uint16(blockAlign)); err != nil {
		return err
	}
	// bits per sample
	if err := e.AddLE(uint16(e.BitDepth)); err != nil {
		return fmt.Errorf("error encoding bits per sample - %w", err)
	}

	e.wroteHeader = true
	return nil
}

// startPCMChunk writes the data chunk ID and its 32-bit size placeholder.
func (e *Encoder) startPCMChunk() error {
	if err := e.AddLE(riff.DataFormatID); err != nil {
		return fmt.Errorf("error encoding sound header %w", err)
	}
	// write a temporary chunksize
	e.pcmChunkSizePos = e.writtenBytes
	if err := e.AddLE(placeholder32); err != nil {
		return fmt.Errorf("%w when writing wav data chunk size header", err)
	}
	e.pcmDataStart = e.writtenBytes
	e.pcmChunkStarted = true
	return nil
}

// checkModeLocked verifies that the container mode wasn't changed after the
// header was written.
func (e *Encoder) checkModeLocked() error {
	if e.wroteHeader && e.RF64 != e.rf64 {
		return ErrModeLocked
	}
	return nil
}

// poison records an error that makes the produced file unusable, so Close
// won't report it as a success later.
func (e *Encoder) poison(err error) {
	if err != nil && e.err == nil {
		e.err = err
	}
}

// Write encodes and writes the passed buffer to the underlying writer.
// Don't forget to Close() the encoder or the file won't be valid.
func (e *Encoder) Write(buf *audio.IntBuffer) error {
	if e == nil {
		return fmt.Errorf("can't write a nil encoder")
	}
	if e.closed {
		return ErrClosed
	}
	if e.err != nil {
		return e.err
	}
	if err := e.checkModeLocked(); err != nil {
		return err
	}
	// Validate the buffer before any header bytes are emitted so a caller
	// mistake doesn't leave a partial file behind.
	if err := e.validateBuffer(buf); err != nil {
		return err
	}

	if !e.wroteHeader {
		if err := e.writeHeader(); err != nil {
			// A writer that can't seek or a broken header makes it impossible
			// to produce a correct file.
			if errors.Is(err, ErrUnseekable) {
				e.poison(err)
			}
			return err
		}
	}
	if err := e.checkModeLocked(); err != nil {
		return err
	}
	if !e.pcmChunkStarted {
		if err := e.startPCMChunk(); err != nil {
			return err
		}
	}
	if err := e.addBuffer(buf); err != nil {
		// A data size the container can't represent poisons the encoder: the
		// intended file can no longer be produced, so Close must not report
		// success. Writer I/O errors are recorded by write itself; other
		// input validation errors stay retryable.
		if errors.Is(err, ErrSizeOverflow) {
			e.poison(err)
		}
		return err
	}
	return nil
}

// WriteFrame writes a single frame of data to the underlying writer.
// The value must serialize to exactly one frame, i.e. blockAlign bytes.
func (e *Encoder) WriteFrame(value interface{}) error {
	if e == nil {
		return fmt.Errorf("can't write a nil encoder")
	}
	if e.closed {
		return ErrClosed
	}
	if e.err != nil {
		return e.err
	}
	if err := e.checkModeLocked(); err != nil {
		return err
	}

	// Validate the frame before emitting the header so a caller mistake can't
	// leave a header-only file that Close would report as a success.
	frameSize := binary.Size(value)
	if frameSize < 0 {
		return fmt.Errorf("wav: can't encode a value of type %T", value)
	}
	blockAlign, err := e.blockAlign()
	if err != nil {
		return err
	}
	if frameSize != blockAlign {
		return fmt.Errorf("wav: WriteFrame expects one frame of %d bytes, got %d", blockAlign, frameSize)
	}

	if !e.wroteHeader {
		if err := e.writeHeader(); err != nil {
			if errors.Is(err, ErrUnseekable) {
				e.poison(err)
			}
			return err
		}
	}
	if err := e.checkModeLocked(); err != nil {
		return err
	}
	if !e.pcmChunkStarted {
		if err := e.startPCMChunk(); err != nil {
			return err
		}
	}

	if err := e.checkFrameDelta(1); err != nil {
		if errors.Is(err, ErrSizeOverflow) {
			e.poison(err)
		}
		return err
	}
	if err := e.AddLE(value); err != nil {
		return err
	}
	e.frames++
	return nil
}

func (e *Encoder) writeMetadata() error {
	chunkData := encodeInfoChunk(e)
	if err := e.AddBE(CIDList); err != nil {
		return fmt.Errorf("failed to write the LIST chunk ID: %w", err)
	}
	if err := e.AddLE(uint32(len(chunkData))); err != nil {
		return fmt.Errorf("failed to write the LIST chunk size: %w", err)
	}
	if err := e.AddBE(chunkData); err != nil {
		return err
	}
	// The RIFF chunk sizes exclude the padding byte, but all chunks must be
	// word aligned. RF64 files written by this encoder add it; classic RIFF
	// keeps the historical byte layout (LIST chunks produced here are word
	// aligned in practice because encodeInfoChunk emits 4+4 sized entries).
	if e.rf64 && (8+len(chunkData))%2 == 1 {
		if err := e.write([]byte{0}); err != nil {
			return err
		}
	}
	return nil
}

// writeAt seeks to offset and writes the serialized value without changing the
// streaming byte counters.
func (e *Encoder) writeAt(offset int64, value interface{}) error {
	if _, err := e.w.Seek(offset, io.SeekStart); err != nil {
		return fmt.Errorf("%w: %v", ErrUnseekable, err)
	}
	var b bytes.Buffer
	if err := binary.Write(&b, binary.LittleEndian, value); err != nil {
		return err
	}
	n, err := e.w.Write(b.Bytes())
	if err != nil {
		return err
	}
	if n != b.Len() {
		return io.ErrShortWrite
	}
	return nil
}

// Close flushes the content to disk, make sure the headers are up to date.
// Note that the underlying writer is NOT being closed.
//
// Close is idempotent: repeated calls return the same result as the first
// call and never append the trailer or metadata twice. It returns ErrNoData if
// no audio data was ever written, ErrSizeOverflow if a classic RIFF file would
// exceed its 4 GiB limit, and an error wrapping ErrUnseekable if the sizes
// can't be backfilled. After a failed Write, Close returns that error instead
// of reporting the partial file as a success.
func (e *Encoder) Close() error {
	if e == nil || e.w == nil {
		return nil
	}
	if e.closed {
		return e.closeErr
	}
	e.closed = true

	if e.err != nil {
		e.closeErr = fmt.Errorf("wav: close after a write error: %w", e.err)
		return e.closeErr
	}
	if !e.wroteHeader {
		e.closeErr = ErrNoData
		return e.closeErr
	}
	if err := e.checkModeLocked(); err != nil {
		e.closeErr = err
		return e.closeErr
	}
	dataBytes, err := e.dataBytes()
	if err != nil {
		e.closeErr = err
		return e.closeErr
	}

	// The trailer belongs right after the data chunk. Seeking there (instead
	// of assuming the current position) keeps the layout correct when the data
	// chunk is longer than what a sparse writer physically stored.
	if e.pcmChunkStarted {
		if _, err := e.w.Seek(e.baseOffset+e.pcmDataStart+dataBytes, io.SeekStart); err != nil {
			e.closeErr = fmt.Errorf("%w: %v", ErrUnseekable, err)
			return e.closeErr
		}
	}

	// All chunks must be word aligned. Only RF64 adds the padding byte here;
	// the classic RIFF output is kept byte-for-byte identical to the previous
	// implementation.
	padBytes := int64(0)
	if e.rf64 && dataBytes%2 == 1 {
		padBytes = 1
		if err := e.write([]byte{0}); err != nil {
			e.closeErr = fmt.Errorf("wav: failed to write the data chunk padding: %w", err)
			return e.closeErr
		}
	}

	// inject metadata at the end to not trip implementation not supporting
	// metadata chunks
	metadataBytes := int64(0)
	if e.Metadata != nil {
		chunkData := encodeInfoChunk(e)
		// LIST ID + 32-bit size + payload.
		metadataBytes = int64(8 + len(chunkData))
		// RF64 chunks are word aligned; the historical RIFF layout is kept
		// byte-for-byte identical and doesn't add a trailing pad byte.
		if e.rf64 && metadataBytes%2 == 1 {
			metadataBytes++
		}
		if err := e.writeMetadata(); err != nil {
			e.closeErr = fmt.Errorf("failed to write metadata - %w", err)
			return e.closeErr
		}
	}

	// pcmDataStart is relative to the start of the container, so the total
	// equals the data start plus the (possibly virtual) PCM bytes and trailer.
	var totalSize int64
	if e.pcmChunkStarted {
		fixed := e.pcmDataStart + padBytes + metadataBytes
		if dataBytes > maxInt64Value-fixed {
			e.closeErr = fmt.Errorf("%w: container size overflow", ErrSizeOverflow)
			return e.closeErr
		}
		totalSize = fixed + dataBytes
	} else {
		// Legacy path: the caller wrote the whole container by hand, so the
		// container is exactly the bytes emitted by AddLE/AddBE.
		totalSize = e.writtenBytes
	}
	if totalSize < 8 {
		e.closeErr = fmt.Errorf("wav: inconsistent container size %d", totalSize)
		return e.closeErr
	}

	if e.rf64 {
		if err := e.writeAt(e.baseOffset+rf64RiffSizePos, uint64(totalSize-8)); err != nil {
			e.closeErr = fmt.Errorf("wav: failed to write the RF64 riff size: %w", err)
			return e.closeErr
		}
		if err := e.writeAt(e.baseOffset+rf64DataSizePos, uint64(dataBytes)); err != nil {
			e.closeErr = fmt.Errorf("wav: failed to write the RF64 data size: %w", err)
			return e.closeErr
		}
		if err := e.writeAt(e.baseOffset+rf64SampleCountPos, uint64(e.frames)); err != nil {
			e.closeErr = fmt.Errorf("wav: failed to write the RF64 sample count: %w", err)
			return e.closeErr
		}
	} else {
		if uint64(totalSize-8) > maxUint32Value || uint64(dataBytes) > maxUint32Value {
			e.closeErr = fmt.Errorf("%w: RIFF file can't exceed 4 GiB, use RF64", ErrSizeOverflow)
			return e.closeErr
		}
		// go back and write total size in header
		if err := e.writeAt(e.baseOffset+4, uint32(totalSize-8)); err != nil {
			e.closeErr = fmt.Errorf("%w when writing the total written bytes", err)
			return e.closeErr
		}
		// rewrite the audio chunk length header
		if e.pcmChunkStarted {
			if err := e.writeAt(e.baseOffset+e.pcmChunkSizePos, uint32(dataBytes)); err != nil {
				e.closeErr = fmt.Errorf("%w when writing wav data chunk size header", err)
				return e.closeErr
			}
		}
	}

	// jump back to the end of the container so the caller can keep appending.
	if e.pcmChunkStarted {
		if _, err := e.w.Seek(e.baseOffset+totalSize, io.SeekStart); err != nil {
			e.closeErr = fmt.Errorf("%w: %v", ErrUnseekable, err)
			return e.closeErr
		}
	} else if _, err := e.w.Seek(0, io.SeekEnd); err != nil {
		// Legacy path: the caller wrote the whole container by hand.
		e.closeErr = fmt.Errorf("%w: %v", ErrUnseekable, err)
		return e.closeErr
	}
	if f, ok := e.w.(*os.File); ok {
		if err := f.Sync(); err != nil {
			e.closeErr = err
			return e.closeErr
		}
	}
	return nil
}
