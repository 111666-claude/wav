// Fixture generator for the adtl (associated data list) support.
//
// It writes two fully hand-crafted WAV files - byte by byte, without using the
// wav encoder - so the decoder tests have an independent reference:
//
//   - fixtures/adtl-after-data.wav: cue + LIST adtl placed after the PCM data
//   - fixtures/adtl-before-data.wav: the same chunks placed between fmt and data
//
// The content mixes even and odd length (UTF-8) strings so every sub chunk and
// the wrapping LIST exercise the zero padding rules.
//
// Run with: go run cmd/gen-adtl-fixtures/main.go
package main

import (
	"encoding/binary"
	"log"
	"os"
	"path/filepath"
)

const (
	sampleRate = 8000
	channels   = 1
	bitDepth   = 16
	numFrames  = 80
)

func main() {
	pcm := makePCM()
	cue := buildCueChunk()
	adtl := buildAdtlList()

	if err := os.MkdirAll("fixtures", 0o755); err != nil {
		log.Fatal(err)
	}
	writeWAV(filepath.Join("fixtures", "adtl-after-data.wav"), pcm, [][]byte{cue, adtl}, true)
	writeWAV(filepath.Join("fixtures", "adtl-before-data.wav"), pcm, [][]byte{cue, adtl}, false)
	log.Println("adtl fixtures generated")
}

// makePCM builds 80 mono 16 bit frames (a simple ramp, 160 bytes total).
func makePCM() []byte {
	pcm := make([]byte, numFrames*2)
	for i := 0; i < numFrames; i++ {
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(i*128))
	}
	return pcm
}

func buildCueChunk() []byte {
	b := newBuilder()
	offsets := []uint32{0, 10, 40}
	b.fourCC('c', 'u', 'e', ' ')
	body := newBuilder()
	body.le32(uint32(len(offsets)))
	for i, off := range offsets {
		body.le32(uint32(i + 1)) // cue ID
		body.le32(off)           // play position
		body.fourCC('d', 'a', 't', 'a')
		body.le32(0) // chunk start
		body.le32(0) // block start
		body.le32(off)
	}
	b.le32(uint32(body.Len()))
	b.Write(body.Bytes())
	return b.Bytes()
}

func buildAdtlList() []byte {
	body := newBuilder()
	body.fourCC('a', 'd', 't', 'l')

	// labl 1: "Intro" (5 bytes + NUL = 6, even -> no padding)
	body.subChunk(fourCCValue('l', 'a', 'b', 'l'), append(cueID(1), []byte("Intro\x00")...))
	// labl 2: "Verse-A" (7 + NUL = 8, even)
	body.subChunk(fourCCValue('l', 'a', 'b', 'l'), append(cueID(2), []byte("Verse-A\x00")...))
	// labl 3: "Coda!" (5 + NUL = 6, even)
	body.subChunk(fourCCValue('l', 'a', 'b', 'l'), append(cueID(3), []byte("Coda!\x00")...))

	// note 1: "多轨注释" : 4 runes, 12 UTF-8 bytes + NUL = 13, odd -> padding
	body.subChunk(fourCCValue('n', 'o', 't', 'e'), append(cueID(1), append([]byte("多轨注释"), 0)...))
	// note 2: "peak" (4 + NUL = 5, odd -> padding)
	body.subChunk(fourCCValue('n', 'o', 't', 'e'), append(cueID(2), append([]byte("peak"), 0)...))

	// ltxt 3: start frame 40 + length 40 = exactly the end of the audio;
	// fixed 20 bytes + "fade out" (8 + NUL = 9) = 29, odd -> padding
	ltxt := newBuilder()
	ltxt.Write(cueID(3))
	ltxt.le32(40) // sample length
	ltxt.fourCC('r', 'g', 'n', ' ')
	ltxt.le16(0)     // country
	ltxt.le16(0)     // language
	ltxt.le16(0)     // dialect
	ltxt.le16(65001) // code page: UTF-8
	ltxt.Write([]byte("fade out\x00"))
	body.subChunk(fourCCValue('l', 't', 'x', 't'), ltxt.Bytes())

	out := newBuilder()
	out.fourCC('L', 'I', 'S', 'T')
	out.le32(uint32(body.Len()))
	out.Write(body.Bytes())
	return out.Bytes()
}

// writeWAV assembles a complete RIFF WAVE container. When metaAfterData is
// true the metadata chunks are appended after the data chunk, otherwise they
// are injected between the fmt and data chunks.
func writeWAV(path string, pcm []byte, meta [][]byte, metaAfterData bool) {
	fmtChunk := newBuilder()
	fmtChunk.fourCC('f', 'm', 't', ' ')
	fmtChunk.le32(16)
	fmtChunk.le16(1) // PCM
	fmtChunk.le16(channels)
	fmtChunk.le32(sampleRate)
	fmtChunk.le32(sampleRate * channels * bitDepth / 8)
	fmtChunk.le16(channels * bitDepth / 8)
	fmtChunk.le16(bitDepth)

	dataChunk := newBuilder()
	dataChunk.fourCC('d', 'a', 't', 'a')
	dataChunk.le32(uint32(len(pcm)))
	dataChunk.Write(pcm)

	inner := newBuilder()
	inner.fourCC('W', 'A', 'V', 'E')
	inner.Write(fmtChunk.Bytes())
	if !metaAfterData {
		for _, c := range meta {
			inner.Write(c)
		}
	}
	inner.Write(dataChunk.Bytes())
	if metaAfterData {
		for _, c := range meta {
			inner.Write(c)
		}
	}

	riff := newBuilder()
	riff.fourCC('R', 'I', 'F', 'F')
	riff.le32(uint32(inner.Len()))
	riff.Write(inner.Bytes())

	if err := os.WriteFile(path, riff.Bytes(), 0o644); err != nil {
		log.Fatal(err)
	}
}

func cueID(n uint32) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, n)
	return b
}

func fourCCValue(a, b, c, d byte) [4]byte {
	return [4]byte{a, b, c, d}
}

// builder is a small little endian byte buffer with RIFF aware helpers.
type builder struct{ buf []byte }

func newBuilder() *builder { return &builder{} }

func (b *builder) Write(p []byte) { b.buf = append(b.buf, p...) }
func (b *builder) Len() int       { return len(b.buf) }
func (b *builder) Bytes() []byte  { return b.buf }

func (b *builder) le16(v uint16) {
	b.buf = binary.LittleEndian.AppendUint16(b.buf, v)
}

func (b *builder) le32(v uint32) {
	b.buf = binary.LittleEndian.AppendUint32(b.buf, v)
}

func (b *builder) fourCC(a, c, d, e byte) {
	b.buf = append(b.buf, a, c, d, e)
}

// subChunk writes a sub chunk header, payload and the mandatory zero padding
// for odd sized payloads.
func (b *builder) subChunk(id [4]byte, payload []byte) {
	b.Write(id[:])
	b.le32(uint32(len(payload)))
	b.Write(payload)
	if len(payload)%2 == 1 {
		b.buf = append(b.buf, 0x00)
	}
}
