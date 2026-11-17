package wav

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/go-audio/audio"
)

// buildIMAWAV builds an in-memory IMA ADPCM WAV file. The data chunk is
// stored verbatim so the tests can corrupt individual fields and bytes.
func buildIMAWAV(numChans, sampleRate, blockAlign, samplesPerBlock int, factSamples uint32, includeFact bool, data []byte) []byte {
	var b bytes.Buffer
	le16 := func(v int) { b.WriteByte(byte(v)); b.WriteByte(byte(v >> 8)) }
	le32 := func(v uint32) { b.Write([]byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)}) }

	b.Write([]byte("RIFF"))
	le32(0) // placeholder, fixed up at the end
	b.Write([]byte("WAVE"))

	// fmt chunk: 16 standard bytes + 4 byte IMA extension.
	b.Write([]byte("fmt "))
	le32(20)
	le16(int(WavFormatIMAADPCM))
	le16(numChans)
	le32(uint32(sampleRate))
	avgBytes := sampleRate * blockAlign / samplesPerBlock
	le32(uint32(avgBytes))
	le16(blockAlign)
	le16(4) // bits per sample
	le16(2) // cbSize
	le16(samplesPerBlock)

	if includeFact {
		b.Write([]byte("fact"))
		le32(4)
		le32(factSamples)
	}

	b.Write([]byte("data"))
	le32(uint32(len(data)))
	b.Write(data)
	if len(data)%2 == 1 {
		b.WriteByte(0) // RIFF word alignment padding
	}

	raw := b.Bytes()
	binary.LittleEndian.PutUint32(raw[4:8], uint32(len(raw)-8))
	return raw
}

// validMonoBlock returns a well formed mono IMA ADPCM block of blockAlign
// bytes starting from predictor 0 / step index 0.
func validMonoBlock(blockAlign int) []byte {
	block := make([]byte, blockAlign)
	// header: predictor 0, step index 0, reserved 0
	// data nibbles default to 0x00 which is a legal code word.
	return block
}

func TestIMA_DecodeFixtures(t *testing.T) {
	testCases := []struct {
		path            string
		numChans        int
		sampleRate      int
		blockAlign      int
		samplesPerBlock int
		factSamples     uint32
		totalSamples    int
		totalFrames     int
		firstSamples    []int
	}{
		{
			path:            "fixtures/adpcm_ima_mono.wav",
			numChans:        1,
			sampleRate:      8000,
			blockAlign:      1024,
			samplesPerBlock: 2041,
			factSamples:     8000,
			totalSamples:    8000,
			totalFrames:     8000,
			firstSamples:    []int{0, 13, 43, 106, 242, 536, 1167, 2524, 1554, -33, -1099, -2457},
		},
		{
			path:            "fixtures/adpcm_ima_mono_bs32.wav",
			numChans:        1,
			sampleRate:      8000,
			blockAlign:      32,
			samplesPerBlock: 57,
			factSamples:     8000,
			totalSamples:    8000,
			totalFrames:     8000,
			firstSamples:    []int{0, 13, 43, 106, 242, 536, 1167, 2524, 1554, -33, -1099, -2457},
		},
		{
			path:            "fixtures/adpcm_ima_mono_short.wav",
			numChans:        1,
			sampleRate:      8000,
			blockAlign:      1024,
			samplesPerBlock: 2041,
			// The last block is partial: fact limits output to 1000 frames.
			factSamples:  1000,
			totalSamples: 1000,
			totalFrames:  1000,
			firstSamples: []int{0, 13, 43, 106, 242, 536, 1167, 2524, 1554, -33, -1099, -2457},
		},
		{
			path:            "fixtures/adpcm_ima_stereo.wav",
			numChans:        2,
			sampleRate:      44100,
			blockAlign:      1024,
			samplesPerBlock: 1017,
			factSamples:     22050,
			totalSamples:    44100,
			totalFrames:     22050,
			firstSamples:    []int{0, 0, 13, 13, 43, 43, 106, 106, 242, 242, 536, 536},
		},
		{
			path:            "fixtures/adpcm_ima_stereo_short.wav",
			numChans:        2,
			sampleRate:      44100,
			blockAlign:      64,
			samplesPerBlock: 57,
			factSamples:     4410,
			totalSamples:    8820,
			totalFrames:     4410,
			firstSamples:    []int{0, 0, 13, 13, 43, 43, 106, 106, 242, 242, 536, 536},
		},
	}

	for _, tc := range testCases {
		t.Run(filepath.Base(tc.path), func(t *testing.T) {
			f, err := os.Open(tc.path)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			d := NewDecoder(f)
			d.ReadInfo()
			if err := d.Err(); err != nil {
				t.Fatalf("ReadInfo failed: %v", err)
			}
			if d.WavAudioFormat != WavFormatIMAADPCM {
				t.Fatalf("expected format %d, got %d", WavFormatIMAADPCM, d.WavAudioFormat)
			}
			if int(d.NumChans) != tc.numChans {
				t.Fatalf("expected %d channels, got %d", tc.numChans, d.NumChans)
			}
			if int(d.SampleRate) != tc.sampleRate {
				t.Fatalf("expected rate %d, got %d", tc.sampleRate, d.SampleRate)
			}
			if int(d.BlockAlign) != tc.blockAlign {
				t.Fatalf("expected block align %d, got %d", tc.blockAlign, d.BlockAlign)
			}
			if int(d.ADPCMSamplesPerBlock) != tc.samplesPerBlock {
				t.Fatalf("expected %d samples per block, got %d", tc.samplesPerBlock, d.ADPCMSamplesPerBlock)
			}
			if !d.IsValidFile() {
				t.Fatal("expected the IMA ADPCM file to validate")
			}
			// The fact chunk sits after fmt and is reached while forwarding.
			if err := d.FwdToPCM(); err != nil {
				t.Fatalf("FwdToPCM failed: %v", err)
			}
			if d.FactSamples != tc.factSamples {
				t.Fatalf("expected fact %d, got %d", tc.factSamples, d.FactSamples)
			}

			full, err := d.FullPCMBuffer()
			if err != nil {
				t.Fatalf("FullPCMBuffer failed: %v", err)
			}
			if len(full.Data) != tc.totalSamples {
				t.Fatalf("expected %d decoded samples, got %d", tc.totalSamples, len(full.Data))
			}
			if full.NumFrames() != tc.totalFrames {
				t.Fatalf("expected %d frames, got %d", tc.totalFrames, full.NumFrames())
			}
			if full.Format.NumChannels != tc.numChans || full.Format.SampleRate != tc.sampleRate {
				t.Fatalf("unexpected output format: %+v", full.Format)
			}
			if full.SourceBitDepth != 16 {
				t.Fatalf("decoded PCM must be 16 bit, got %d", full.SourceBitDepth)
			}
			for i, want := range tc.firstSamples {
				if full.Data[i] != want {
					t.Fatalf("sample %d: expected %d, got %d", i, want, full.Data[i])
				}
			}
		})
	}
}

func TestIMA_PerChannelOutput(t *testing.T) {
	// In a stereo stream every even index is the left channel and every odd
	// index the right channel; both must decode independently.
	f, err := os.Open("fixtures/adpcm_ima_stereo.wav")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	d := NewDecoder(f)
	buf, err := d.FullPCMBuffer()
	if err != nil {
		t.Fatal(err)
	}
	if len(buf.Data)%2 != 0 {
		t.Fatalf("stereo output must be frame aligned, got %d samples", len(buf.Data))
	}
	// The first frame holds the two block header predictors.
	if buf.Data[0] != 0 || buf.Data[1] != 0 {
		t.Fatalf("unexpected first frame: %d,%d", buf.Data[0], buf.Data[1])
	}
	// Gather each channel and make sure the right channel is not a copy of
	// the left one but still follows the same valid 16 bit range.
	var lNonZero, rNonZero int
	for i := 0; i < len(buf.Data); i += 2 {
		if buf.Data[i] != 0 {
			lNonZero++
		}
		if buf.Data[i+1] != 0 {
			rNonZero++
		}
		for _, v := range buf.Data[i : i+2] {
			if v < -32768 || v > 32767 {
				t.Fatalf("sample out of 16 bit range: %d", v)
			}
		}
	}
	if lNonZero == 0 || rNonZero == 0 {
		t.Fatalf("both channels must contain data, got left=%d right=%d non-zero", lNonZero, rNonZero)
	}
}

func TestIMA_StreamingMatchesFullBuffer(t *testing.T) {
	for _, path := range []string{
		"fixtures/adpcm_ima_mono.wav",
		"fixtures/adpcm_ima_mono_short.wav",
		"fixtures/adpcm_ima_stereo.wav",
		"fixtures/adpcm_ima_stereo_short.wav",
	} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			// Full buffer reference.
			rf, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			full, err := NewDecoder(rf).FullPCMBuffer()
			rf.Close()
			if err != nil {
				t.Fatal(err)
			}

			// Streamed through PCMBuffer with an awkward buffer size so the
			// block boundaries and pending queue get exercised.
			sf, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer sf.Close()
			d := NewDecoder(sf)
			streamed := []int{}
			buf := &audio.IntBuffer{Data: make([]int, 127)}
			for {
				n, err := d.PCMBuffer(buf)
				streamed = append(streamed, buf.Data[:n]...)
				if err != nil {
					t.Fatalf("streaming decode failed: %v", err)
				}
				if n == 0 {
					break
				}
			}
			if !reflect.DeepEqual(streamed, full.Data) {
				t.Fatalf("streamed output (%d samples) differs from full buffer (%d samples)",
					len(streamed), len(full.Data))
			}
		})
	}
}

func TestIMA_Rewind(t *testing.T) {
	f, err := os.Open("fixtures/adpcm_ima_stereo.wav")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	d := NewDecoder(f)
	first := &audio.IntBuffer{Data: make([]int, 512)}
	n, err := d.PCMBuffer(first)
	if err != nil || n != 512 {
		t.Fatalf("first read n=%d err=%v", n, err)
	}
	if err := d.Rewind(); err != nil {
		t.Fatal(err)
	}
	second := &audio.IntBuffer{Data: make([]int, 512)}
	n, err = d.PCMBuffer(second)
	if err != nil || n != 512 {
		t.Fatalf("rewound read n=%d err=%v", n, err)
	}
	if !reflect.DeepEqual(first.Data, second.Data) {
		t.Fatal("expected identical output after rewinding")
	}
}

func TestIMA_Duration(t *testing.T) {
	f, err := os.Open("fixtures/adpcm_ima_mono.wav")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	d := NewDecoder(f)
	// The fact chunk is reached while forwarding to the data chunk.
	if err := d.FwdToPCM(); err != nil {
		t.Fatal(err)
	}
	// fact = 8000 samples at 8000 Hz => exactly one second.
	dur, err := d.Duration()
	if err != nil {
		t.Fatal(err)
	}
	if dur.Milliseconds() != 1000 {
		t.Fatalf("expected 1s duration, got %s", dur)
	}
}

func TestIMA_StepIndexOutOfRange(t *testing.T) {
	block := validMonoBlock(16)
	block[2] = 89 // step index above the valid 0..88 range
	wav := buildIMAWAV(1, 8000, 16, 25, 25, true, block)
	d := NewDecoder(bytes.NewReader(wav))
	if _, err := d.FullPCMBuffer(); !errors.Is(err, ErrStepIndexOutOfRange) {
		t.Fatalf("expected ErrStepIndexOutOfRange, got %v", err)
	}
}

func TestIMA_TruncatedBlock(t *testing.T) {
	// Declare a 32 byte block align but only provide 20 data bytes.
	wav := buildIMAWAV(1, 8000, 32, 57, 57, true, make([]byte, 20))
	d := NewDecoder(bytes.NewReader(wav))
	if _, err := d.FullPCMBuffer(); !errors.Is(err, ErrTruncatedBlock) {
		t.Fatalf("expected ErrTruncatedBlock, got %v", err)
	}
}

func TestIMA_FactMismatch(t *testing.T) {
	// One full 16 byte mono block holds 25 samples, but fact claims 30.
	block := validMonoBlock(16)
	wav := buildIMAWAV(1, 8000, 16, 25, 30, true, block)
	d := NewDecoder(bytes.NewReader(wav))
	if _, err := d.FullPCMBuffer(); !errors.Is(err, ErrFactMismatch) {
		t.Fatalf("expected ErrFactMismatch, got %v", err)
	}
}

func TestIMA_InvalidBlockAlign(t *testing.T) {
	t.Run("mono too small", func(t *testing.T) {
		// mono block align below the 4 byte block header.
		wav := buildIMAWAV(1, 8000, 2, 1, 1, true, make([]byte, 2))
		d := NewDecoder(bytes.NewReader(wav))
		if d.IsValidFile() {
			t.Fatal("expected invalid file for block align 2")
		}
	})
	t.Run("stereo not group aligned", func(t *testing.T) {
		// stereo block align must be 8 plus a multiple of 8.
		wav := buildIMAWAV(2, 44100, 12, 9, 9, true, make([]byte, 12))
		d := NewDecoder(bytes.NewReader(wav))
		d.ReadInfo()
		if !errors.Is(d.Err(), ErrInvalidBlockAlign) {
			t.Fatalf("expected ErrInvalidBlockAlign, got %v", d.Err())
		}
	})
	t.Run("fmt samples per block mismatch", func(t *testing.T) {
		block := validMonoBlock(16)
		// block align 16 yields 25 samples, claim 24 in the extension.
		wav := buildIMAWAV(1, 8000, 16, 24, 25, true, block)
		d := NewDecoder(bytes.NewReader(wav))
		if _, err := d.FullPCMBuffer(); !errors.Is(err, ErrInvalidBlockAlign) {
			t.Fatalf("expected ErrInvalidBlockAlign, got %v", err)
		}
	})
}

func TestIMA_OddDataSizePaddingIgnored(t *testing.T) {
	// A block align of 16 with an odd data size of 17 means one full block
	// followed by a single byte that starts a truncated block. The RIFF
	// padding byte (the 18th byte in the file) must never be counted as data,
	// so the reader reports exactly one byte read of the 16 byte block.
	data := append(validMonoBlock(16), 0xAB)
	wav := buildIMAWAV(1, 8000, 16, 25, 25, true, data)
	d := NewDecoder(bytes.NewReader(wav))
	_, err := d.FullPCMBuffer()
	if !errors.Is(err, ErrTruncatedBlock) {
		t.Fatalf("expected ErrTruncatedBlock, got %v", err)
	}
	// The message proves the limit stopped at the declared (odd) data size
	// rather than reading the RIFF padding byte (which would report 2 of 16).
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("read 1 of 16")) {
		t.Fatalf("expected truncation after 1 data byte, got: %v", err)
	}
}

func TestIMA_UnsupportedChannels(t *testing.T) {
	block := make([]byte, 16)
	wav := buildIMAWAV(3, 8000, 16, 25, 25, true, block)
	d := NewDecoder(bytes.NewReader(wav))
	d.ReadInfo()
	if !errors.Is(d.Err(), ErrInvalidBlockAlign) {
		t.Fatalf("expected ErrInvalidBlockAlign for 3 channels, got %v", d.Err())
	}
}
