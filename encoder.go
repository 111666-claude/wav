package wav

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"os"
	"time"

	"github.com/go-audio/audio"
	"github.com/go-audio/riff"
)

// Encoder encodes LPCM data into a wav containter.
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
	// This field is ignored when ChannelMask is set.
	WavAudioFormat int

	// ChannelMask enables WAVE_FORMAT_EXTENSIBLE output when it is non-zero.
	// It describes the speaker position of each interleaved channel (the
	// number of set bits must equal NumChans, see DefaultChannelMask for
	// common layouts). When zero, the encoder writes a standard 16 byte
	// "fmt " chunk using WavAudioFormat and the legacy behavior is unchanged.
	ChannelMask uint32

	// ValidBitsPerSample is the wValidBitsPerSample value written in the
	// WAVE_FORMAT_EXTENSIBLE extension. When zero, it defaults to BitDepth.
	// It must not be greater than BitDepth. Only used when ChannelMask is set.
	ValidBitsPerSample int

	// Metadata contains metadata to inject in the file.
	Metadata *Metadata

	WrittenBytes    int
	frames          int
	pcmChunkStarted bool
	pcmChunkSizePos int
	wroteHeader     bool // true if we've written the header out
}

// NewEncoder creates a new encoder to create a new wav file.
// Don't forget to add Frames to the encoder before writing.
func NewEncoder(w io.WriteSeeker, sampleRate, bitDepth, numChans, audioFormat int) *Encoder {
	return &Encoder{
		w:              w,
		buf:            bytes.NewBuffer(make([]byte, 0, bytesNumFromDuration(time.Minute, sampleRate, bitDepth)*numChans)),
		SampleRate:     sampleRate,
		BitDepth:       bitDepth,
		NumChans:       numChans,
		WavAudioFormat: audioFormat,
	}
}

// AddLE serializes and adds the passed value using little endian
func (e *Encoder) AddLE(src interface{}) error {
	e.WrittenBytes += binary.Size(src)
	return binary.Write(e.w, binary.LittleEndian, src)
}

// AddBE serializes and adds the passed value using big endian
func (e *Encoder) AddBE(src interface{}) error {
	e.WrittenBytes += binary.Size(src)
	return binary.Write(e.w, binary.BigEndian, src)
}

func (e *Encoder) addBuffer(buf *audio.IntBuffer) error {
	if buf == nil {
		return fmt.Errorf("can't add a nil buffer")
	}

	frameCount := buf.NumFrames()
	// performance tweak: setup a buffer so we don't do too many writes
	var err error
	for i := 0; i < frameCount; i++ {
		for j := 0; j < buf.Format.NumChannels; j++ {
			v := buf.Data[i*buf.Format.NumChannels+j]
			switch e.BitDepth {
			case 8:
				if err = binary.Write(e.buf, binary.LittleEndian, uint8(v)); err != nil {
					return err
				}
			case 16:
				if err = binary.Write(e.buf, binary.LittleEndian, int16(v)); err != nil {
					return err
				}
			case 24:
				if err = binary.Write(e.buf, binary.LittleEndian, audio.Int32toInt24LEBytes(int32(v))); err != nil {
					return err
				}
			case 32:
				if err = binary.Write(e.buf, binary.LittleEndian, int32(v)); err != nil {
					return err
				}
			default:
				return fmt.Errorf("can't add frames of bit size %d", e.BitDepth)
			}
		}
		e.frames++
	}
	if n, err := e.w.Write(e.buf.Bytes()); err != nil {
		e.WrittenBytes += n
		return err
	}
	e.WrittenBytes += e.buf.Len()
	e.buf.Reset()

	return nil
}

func (e *Encoder) writeHeader() error {
	if e.wroteHeader {
		return errors.New("already wrote header")
	}
	e.wroteHeader = true
	if e == nil {
		return fmt.Errorf("can't write a nil encoder")
	}
	if e.w == nil {
		return fmt.Errorf("can't write to a nil writer")
	}

	if e.WrittenBytes > 0 {
		return nil
	}

	// riff ID
	if err := e.AddLE(riff.RiffID); err != nil {
		return err
	}
	// file size uint32, to update later on.
	if err := e.AddLE(uint32(4294967295)); err != nil {
		return err
	}
	// wave headers
	if err := e.AddLE(riff.WavFormatID); err != nil {
		return err
	}
	extensible := e.ChannelMask != 0
	if extensible {
		// WAVE_FORMAT_EXTENSIBLE only carries integer PCM samples in this
		// implementation, so validate the extension fields up front.
		if bits.OnesCount32(e.ChannelMask) != e.NumChans {
			return fmt.Errorf("%w: mask %#010x sets %d bits but the encoder has %d channels",
				ErrChannelMaskMismatch, e.ChannelMask, bits.OnesCount32(e.ChannelMask), e.NumChans)
		}
		validBits := e.ValidBitsPerSample
		if validBits == 0 {
			validBits = e.BitDepth
		}
		if validBits > e.BitDepth {
			return fmt.Errorf("%w: %d valid bits in a %d bit container",
				ErrValidBitsExceedsContainer, validBits, e.BitDepth)
		}
	}

	// form
	if err := e.AddLE(riff.FmtID); err != nil {
		return err
	}
	// chunk size: 16 bytes for the standard PCM header, 40 bytes when the
	// WAVE_FORMAT_EXTENSIBLE extension (24 extra bytes) is present.
	fmtChunkSize := uint32(16)
	if extensible {
		fmtChunkSize = 40
	}
	if err := e.AddLE(fmtChunkSize); err != nil {
		return err
	}
	// wave format
	formatTag := uint16(e.WavAudioFormat)
	if extensible {
		formatTag = WAVFormatExtensible
	}
	if err := e.AddLE(formatTag); err != nil {
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
	blockAlign := e.NumChans * e.BitDepth / 8
	// avg bytes per sec
	if err := e.AddLE(uint32(e.SampleRate * blockAlign)); err != nil {
		return fmt.Errorf("error encoding the avg bytes per sec - %w", err)
	}
	// block align
	if err := e.AddLE(uint16(blockAlign)); err != nil {
		return err
	}
	// bits per sample (container size)
	if err := e.AddLE(uint16(e.BitDepth)); err != nil {
		return fmt.Errorf("error encoding bits per sample - %w", err)
	}
	if extensible {
		if err := e.writeExtensibleFmt(); err != nil {
			return err
		}
	}

	return nil
}

// Write encodes and writes the passed buffer to the underlying writer.
// Don't forget to Close() the encoder or the file won't be valid.
func (e *Encoder) Write(buf *audio.IntBuffer) error {
	if !e.wroteHeader {
		if err := e.writeHeader(); err != nil {
			return err
		}
	}

	if !e.pcmChunkStarted {
		// sound header
		if err := e.AddLE(riff.DataFormatID); err != nil {
			return fmt.Errorf("error encoding sound header %w", err)
		}
		e.pcmChunkStarted = true

		// write a temporary chunksize
		e.pcmChunkSizePos = e.WrittenBytes
		if err := e.AddLE(uint32(4294967295)); err != nil {
			return fmt.Errorf("%w when writing wav data chunk size header", err)
		}
	}

	return e.addBuffer(buf)
}

// WriteFrame writes a single frame of data to the underlying writer.
func (e *Encoder) WriteFrame(value interface{}) error {
	if !e.wroteHeader {
		if err := e.writeHeader(); err != nil {
			return err
		}
	}
	if !e.pcmChunkStarted {
		// sound header
		if err := e.AddLE(riff.DataFormatID); err != nil {
			return fmt.Errorf("error encoding sound header %w", err)
		}
		e.pcmChunkStarted = true

		// write a temporary chunksize
		e.pcmChunkSizePos = e.WrittenBytes
		if err := e.AddLE(uint32(4294967295)); err != nil {
			return fmt.Errorf("%w when writing wav data chunk size header", err)
		}
	}

	e.frames++
	return e.AddLE(value)
}

func (e *Encoder) writeMetadata() error {
	chunkData := encodeInfoChunk(e)
	if err := e.AddBE(CIDList); err != nil {
		return fmt.Errorf("failed to write the LIST chunk ID: %w", err)
	}
	if err := e.AddLE(uint32(len(chunkData))); err != nil {
		return fmt.Errorf("failed to write the LIST chunk size: %w", err)
	}
	return e.AddBE(chunkData)
}

// Close flushes the content to disk, make sure the headers are up to date
// Note that the underlying writer is NOT being closed.
func (e *Encoder) Close() error {
	if e == nil || e.w == nil {
		return nil
	}

	// inject metadata at the end to not trip implementation not supporting
	// metadata chunks
	if e.Metadata != nil {
		if err := e.writeMetadata(); err != nil {
			return fmt.Errorf("failed to write metadata - %w", err)
		}
	}

	// go back and write total size in header
	if _, err := e.w.Seek(4, 0); err != nil {
		return err
	}
	if err := e.AddLE(uint32(e.WrittenBytes) - 8); err != nil {
		return fmt.Errorf("%w when writing the total written bytes", err)
	}

	// rewrite the audio chunk length header
	if e.pcmChunkSizePos > 0 {
		if _, err := e.w.Seek(int64(e.pcmChunkSizePos), 0); err != nil {
			return err
		}
		chunksize := uint32((int(e.BitDepth) / 8) * int(e.NumChans) * e.frames)
		if err := e.AddLE(uint32(chunksize)); err != nil {
			return fmt.Errorf("%w when writing wav data chunk size header", err)
		}
	}

	// jump back to the end of the file.
	if _, err := e.w.Seek(0, 2); err != nil {
		return err
	}
	switch e.w.(type) {
	case *os.File:
		return e.w.(*os.File).Sync()
	}
	return nil
}
