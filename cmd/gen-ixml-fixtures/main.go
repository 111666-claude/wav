// gen-ixml-fixtures builds the binary iXML WAV fixtures used by the test
// suite. The RIFF container is assembled manually (no dependency on the wav
// package being tested) so the fixtures act as an independent oracle for
// chunk layout, sizes and padding.
//
// Usage: go run ./cmd/gen-ixml-fixtures
package main

import (
	"encoding/binary"
	"log"
	"os"
	"path/filepath"
)

var le = binary.LittleEndian

func chunk(id [4]byte, body []byte) []byte {
	out := make([]byte, 0, 8+len(body)+1)
	out = append(out, id[:]...)
	size := make([]byte, 4)
	le.PutUint32(size, uint32(len(body)))
	out = append(out, size...)
	out = append(out, body...)
	if len(body)%2 == 1 {
		out = append(out, 0x00) // RIFF word alignment padding
	}
	return out
}

func fmtChunk(channels, bits uint16, rate uint32) []byte {
	body := make([]byte, 16)
	le.PutUint16(body[0:], 1) // PCM
	le.PutUint16(body[2:], channels)
	le.PutUint32(body[4:], rate)
	blockAlign := channels * bits / 8
	le.PutUint32(body[8:], rate*uint32(blockAlign))
	le.PutUint16(body[12:], blockAlign)
	le.PutUint16(body[14:], bits)
	return chunk([4]byte{'f', 'm', 't', ' '}, body)
}

func dataChunk(samples []byte) []byte {
	return chunk([4]byte{'d', 'a', 't', 'a'}, samples)
}

// ixmlEven is a 2 track iXML document. Its length is even so no padding byte
// follows it. It includes an unknown top level node (USER) and an unknown
// TRACK child to exercise node retention.
const ixmlEven = `<?xml version="1.0" encoding="UTF-8"?>
<BWFXML>
  <IXML_VERSION>2.0</IXML_VERSION>
  <PROJECT>Field Recordings</PROJECT>
  <SCENE>12A</SCENE>
  <TAKE>3</TAKE>
  <TRACK_LIST>
    <TRACK_COUNT>2</TRACK_COUNT>
    <TRACK>
      <CHANNEL_INDEX>1</CHANNEL_INDEX>
      <INTERLEAVE_INDEX>1</INTERLEAVE_INDEX>
      <NAME>Boom</NAME>
      <FUNCTION>M-MID_SIDE</FUNCTION>
    </TRACK>
    <TRACK>
      <CHANNEL_INDEX>2</CHANNEL_INDEX>
      <INTERLEAVE_INDEX>2</INTERLEAVE_INDEX>
      <NAME>Lav 1</NAME>
      <NOTE>plant mics</NOTE>
    </TRACK>
  </TRACK_LIST>
  <USER>
    <SOUND_ROLL>A001</SOUND_ROLL>
  </USER>
</BWFXML>`

// ixmlOdd is a single track iXML document tuned to have an odd byte length so
// the chunk must be followed by a zero padding byte.
const ixmlOdd = `<BWFXML><IXML_VERSION>1.27</IXML_VERSION><PROJECT>P</PROJECT><SCENE>S</SCENE><TAKE>T1</TAKE><TRACK_LIST><TRACK_COUNT>1</TRACK_COUNT><TRACK><CHANNEL_INDEX>1</CHANNEL_INDEX><INTERLEAVE_INDEX>1</INTERLEAVE_INDEX><NAME>X</NAME></TRACK></TRACK_LIST></BWFXML>`

func writeWAV(path string, chunks ...[]byte) {
	waveID := [4]byte{'W', 'A', 'V', 'E'}
	var body []byte
	body = append(body, waveID[:]...)
	for _, c := range chunks {
		body = append(body, c...)
	}
	riffID := [4]byte{'R', 'I', 'F', 'F'}
	var out []byte
	out = append(out, riffID[:]...)
	hdr := make([]byte, 4)
	le.PutUint32(hdr, uint32(len(body)))
	out = append(out, hdr...)
	out = append(out, body...)
	if err := os.WriteFile(path, out, 0o644); err != nil {
		log.Fatal(err)
	}
	log.Printf("wrote %s (%d bytes)", path, len(out))
}

func main() {
	if len(ixmlEven)%2 != 0 {
		log.Fatalf("ixmlEven must have an even byte length, got %d", len(ixmlEven))
	}
	if len(ixmlOdd)%2 != 1 {
		log.Fatalf("ixmlOdd must have an odd byte length, got %d", len(ixmlOdd))
	}
	dir := filepath.Join("..", "..", "fixtures")

	// 16 bit stereo, 4 frames of distinct PCM so round trips can be checked.
	stereoPCM := []byte{
		0x01, 0x00, 0x02, 0x00, // frame 1: ch1=1,  ch2=2
		0xff, 0x7f, 0x00, 0x80, // frame 2: ch1=32767, ch2=-32768
		0x10, 0x20, 0x30, 0x40, // frame 3
		0xaa, 0x55, 0x55, 0xaa, // frame 4
	}
	// iXML BEFORE the data chunk, even sized payload.
	writeWAV(filepath.Join(dir, "ixml-multitrack.wav"),
		fmtChunk(2, 16, 48000),
		chunk([4]byte{'i', 'X', 'M', 'L'}, []byte(ixmlEven)),
		dataChunk(stereoPCM),
	)

	// 16 bit mono, 3 frames; iXML AFTER the data chunk, odd sized payload + pad.
	monoPCM := []byte{
		0x00, 0x10, // frame 1
		0x00, 0x20, // frame 2
		0x00, 0x30, // frame 3
	}
	writeWAV(filepath.Join(dir, "ixml-odd-footer.wav"),
		fmtChunk(1, 16, 44100),
		dataChunk(monoPCM),
		chunk([4]byte{'i', 'X', 'M', 'L'}, []byte(ixmlOdd)),
	)
}
