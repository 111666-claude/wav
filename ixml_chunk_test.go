package wav

import (
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-audio/audio"
)

// --- raw RIFF helpers, independent of the code under test -------------------

func leU32(n uint32) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, n)
	return b
}

func rawChunk(id [4]byte, body []byte) []byte {
	out := append(append([]byte{}, id[:]...), leU32(uint32(len(body)))...)
	out = append(out, body...)
	if len(body)%2 == 1 {
		out = append(out, 0x00)
	}
	return out
}

func rawFmtChunk(channels, bits uint16, rate uint32) []byte {
	body := make([]byte, 16)
	binary.LittleEndian.PutUint16(body[0:], 1)
	binary.LittleEndian.PutUint16(body[2:], channels)
	binary.LittleEndian.PutUint32(body[4:], rate)
	blockAlign := channels * bits / 8
	binary.LittleEndian.PutUint32(body[8:], rate*uint32(blockAlign))
	binary.LittleEndian.PutUint16(body[12:], blockAlign)
	binary.LittleEndian.PutUint16(body[14:], bits)
	return rawChunk([4]byte{'f', 'm', 't', ' '}, body)
}

// buildRawWAV assembles a WAV file from raw chunks without using the encoder.
func buildRawWAV(chunks ...[]byte) []byte {
	body := append([]byte{}, "WAVE"...)
	for _, c := range chunks {
		body = append(body, c...)
	}
	out := append([]byte{}, "RIFF"...)
	out = append(out, leU32(uint32(len(body)))...)
	return append(out, body...)
}

// rawChunkLayout walks a RIFF/WAVE byte slice and reports each chunk.
type rawChunkInfo struct {
	id     string
	size   uint32
	at     int
	pad    byte
	hasPad bool
}

func walkChunks(t *testing.T, data []byte) []rawChunkInfo {
	t.Helper()
	if got := string(data[:4]); got != "RIFF" {
		t.Fatalf("expected RIFF header, got %q", got)
	}
	riffSize := binary.LittleEndian.Uint32(data[4:8])
	if int(riffSize) != len(data)-8 {
		t.Fatalf("RIFF size field %d does not match body length %d", riffSize, len(data)-8)
	}
	if got := string(data[8:12]); got != "WAVE" {
		t.Fatalf("expected WAVE form, got %q", got)
	}
	var infos []rawChunkInfo
	off := 12
	for off < len(data) {
		if len(data)-off < 8 {
			t.Fatalf("truncated chunk header at offset %d", off)
		}
		id := string(data[off : off+4])
		size := binary.LittleEndian.Uint32(data[off+4 : off+8])
		info := rawChunkInfo{id: id, size: size, at: off}
		end := off + 8 + int(size)
		if size%2 == 1 {
			if end >= len(data) {
				t.Fatalf("chunk %s declares padding past end of file", id)
			}
			info.pad = data[end]
			info.hasPad = true
			if info.pad != 0 {
				t.Fatalf("chunk %s padding byte is %#x, expected 0", id, info.pad)
			}
			end++
		}
		if end > len(data) {
			t.Fatalf("chunk %s (size %d) extends past end of file", id, size)
		}
		infos = append(infos, info)
		off = end
	}
	if off != len(data) {
		t.Fatalf("trailing %d bytes after last chunk", len(data)-off)
	}
	return infos
}

func findChunk(data []byte, want string) (info rawChunkInfo, payload []byte, ok bool) {
	off := 12
	for off < len(data) {
		id := string(data[off : off+4])
		size := int(binary.LittleEndian.Uint32(data[off+4 : off+8]))
		end := off + 8 + size + size%2
		if id == want {
			return rawChunkInfo{id: id, size: uint32(size), at: off}, data[off+8 : off+8+size], true
		}
		off = end
	}
	return rawChunkInfo{}, nil, false
}

// --- fixture binary layout --------------------------------------------------

func TestIXMLFixtures_BinaryLayout(t *testing.T) {
	t.Run("multitrack iXML before data, even payload", func(t *testing.T) {
		data, err := os.ReadFile(filepath.Join("fixtures", "ixml-multitrack.wav"))
		if err != nil {
			t.Fatal(err)
		}
		infos := walkChunks(t, data)
		var order []string
		for _, c := range infos {
			order = append(order, c.id)
		}
		wantOrder := []string{"fmt ", "iXML", "data"}
		if strings.Join(order, ",") != strings.Join(wantOrder, ",") {
			t.Fatalf("chunk order = %v, want %v", order, wantOrder)
		}
		var ix rawChunkInfo
		for _, c := range infos {
			if c.id == "iXML" {
				ix = c
			}
		}
		if ix.size%2 != 0 {
			t.Fatalf("expected even iXML payload, got size %d", ix.size)
		}
		if ix.hasPad {
			t.Fatalf("even payload must not have a padding byte")
		}
		if _, payload, ok := findChunk(data, "iXML"); !ok || !bytes.HasPrefix(payload, []byte(xml.Header)) {
			t.Fatalf("iXML payload does not start with an XML declaration")
		}
	})

	t.Run("footer iXML after data, odd payload with pad byte", func(t *testing.T) {
		data, err := os.ReadFile(filepath.Join("fixtures", "ixml-odd-footer.wav"))
		if err != nil {
			t.Fatal(err)
		}
		infos := walkChunks(t, data)
		var order []string
		for _, c := range infos {
			order = append(order, c.id)
		}
		wantOrder := []string{"fmt ", "data", "iXML"}
		if strings.Join(order, ",") != strings.Join(wantOrder, ",") {
			t.Fatalf("chunk order = %v, want %v", order, wantOrder)
		}
		var ix rawChunkInfo
		for _, c := range infos {
			if c.id == "iXML" {
				ix = c
			}
		}
		if ix.size%2 != 1 {
			t.Fatalf("expected odd iXML payload, got size %d", ix.size)
		}
		if !ix.hasPad || ix.pad != 0 {
			t.Fatalf("odd payload must be followed by a zero pad byte, got %+v", ix)
		}
		// the byte right after the payload (the pad) must be outside the size
		info, payload, ok := findChunk(data, "iXML")
		if !ok {
			t.Fatal("iXML chunk not found")
		}
		if int(info.size) != len(payload) {
			t.Fatalf("size field %d differs from payload length %d", info.size, len(payload))
		}
		if data[info.at+8+int(info.size)] != 0 {
			t.Fatalf("expected zero pad at offset %d", info.at+8+int(info.size))
		}
	})
}

// --- decoding ---------------------------------------------------------------

func expectedMultitrackIXML() *IXMLMetadata {
	return &IXMLMetadata{
		Version: "2.0",
		Project: "Field Recordings",
		Scene:   "12A",
		Take:    "3",
		Tracks: []*IXMLTrack{
			{ChannelIndex: 1, InterleaveIndex: 1, Name: "Boom", Function: "M-MID_SIDE"},
			{ChannelIndex: 2, InterleaveIndex: 2, Name: "Lav 1", Extra: map[string]string{
				"NOTE": "<NOTE>plant mics</NOTE>",
			}},
		},
		Extra: map[string]string{
			"USER": "<USER><SOUND_ROLL>A001</SOUND_ROLL></USER>",
		},
	}
}

func assertIXMLEqual(t *testing.T, got, want *IXMLMetadata) {
	t.Helper()
	if got == nil {
		t.Fatal("expected iXML metadata, got nil")
	}
	if got.Version != want.Version {
		t.Errorf("Version = %q, want %q", got.Version, want.Version)
	}
	if got.Project != want.Project {
		t.Errorf("Project = %q, want %q", got.Project, want.Project)
	}
	if got.Scene != want.Scene {
		t.Errorf("Scene = %q, want %q", got.Scene, want.Scene)
	}
	if got.Take != want.Take {
		t.Errorf("Take = %q, want %q", got.Take, want.Take)
	}
	if len(got.Tracks) != len(want.Tracks) {
		t.Fatalf("track count = %d, want %d", len(got.Tracks), len(want.Tracks))
	}
	for i, tr := range want.Tracks {
		got := got.Tracks[i]
		if got.ChannelIndex != tr.ChannelIndex {
			t.Errorf("track %d ChannelIndex = %d, want %d", i, got.ChannelIndex, tr.ChannelIndex)
		}
		if got.InterleaveIndex != tr.InterleaveIndex {
			t.Errorf("track %d InterleaveIndex = %d, want %d", i, got.InterleaveIndex, tr.InterleaveIndex)
		}
		if got.Name != tr.Name {
			t.Errorf("track %d Name = %q, want %q", i, got.Name, tr.Name)
		}
		if got.Function != tr.Function {
			t.Errorf("track %d Function = %q, want %q", i, got.Function, tr.Function)
		}
		for tag, wantRaw := range tr.Extra {
			if gotRaw := got.Extra[tag]; gotRaw != wantRaw {
				t.Errorf("track %d extra %s = %q, want %q", i, tag, gotRaw, wantRaw)
			}
		}
	}
	for tag, wantRaw := range want.Extra {
		gotRaw, ok := got.Extra[tag]
		if !ok {
			t.Errorf("missing retained top level node %q", tag)
			continue
		}
		if compactXML(t, gotRaw) != compactXML(t, wantRaw) {
			t.Errorf("extra node %s = %q, want %q", tag, gotRaw, wantRaw)
		}
	}
}

// compactXML re-emits an XML fragment with all indentation and inter-element
// whitespace removed so retained nodes can be compared regardless of the
// source document's formatting.
func compactXML(t *testing.T, in string) string {
	t.Helper()
	dec := xml.NewDecoder(strings.NewReader(in))
	var b strings.Builder
	enc := xml.NewEncoder(&b)
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("compactXML parse %q: %v", in, err)
		}
		if cd, ok := tok.(xml.CharData); ok {
			if len(bytes.TrimSpace(cd)) == 0 {
				continue
			}
		}
		if err := enc.EncodeToken(tok); err != nil {
			t.Fatalf("compactXML encode: %v", err)
		}
	}
	if err := enc.Flush(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestIXMLDecoder_Fixtures(t *testing.T) {
	t.Run("multitrack header iXML via FwdToPCM", func(t *testing.T) {
		f, err := os.Open(filepath.Join("fixtures", "ixml-multitrack.wav"))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		d := NewDecoder(f)
		if err := d.FwdToPCM(); err != nil {
			t.Fatalf("FwdToPCM failed: %v", err)
		}
		if d.Metadata == nil || d.Metadata.IXML == nil {
			t.Fatal("iXML before data was not decoded")
		}
		assertIXMLEqual(t, d.Metadata.IXML, expectedMultitrackIXML())
	})

	t.Run("footer iXML via ReadMetadata", func(t *testing.T) {
		f, err := os.Open(filepath.Join("fixtures", "ixml-odd-footer.wav"))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		d := NewDecoder(f)
		d.ReadMetadata()
		if err := d.Err(); err != nil {
			t.Fatalf("ReadMetadata failed: %v", err)
		}
		if d.Metadata == nil || d.Metadata.IXML == nil {
			t.Fatal("iXML after data was not decoded")
		}
		ix := d.Metadata.IXML
		if ix.Project != "P" || ix.Scene != "S" || ix.Take != "T1" {
			t.Fatalf("unexpected scalar fields: %+v", ix)
		}
		if len(ix.Tracks) != 1 || ix.Tracks[0].Name != "X" ||
			ix.Tracks[0].InterleaveIndex != 1 || ix.Tracks[0].ChannelIndex != 1 {
			t.Fatalf("unexpected track list: %+v", ix.Tracks)
		}
	})

	t.Run("ixml before fmt chunk", func(t *testing.T) {
		doc := []byte(`<BWFXML><SCENE>9</SCENE><TAKE>2</TAKE></BWFXML>`)
		data := buildRawWAV(
			rawChunk(CIDiXML, doc),
			rawFmtChunk(1, 16, 48000),
			rawChunk([4]byte{'d', 'a', 't', 'a'}, []byte{0, 0}),
		)
		d := NewDecoder(bytes.NewReader(data))
		d.ReadMetadata()
		if err := d.Err(); err != nil {
			t.Fatalf("ReadMetadata failed: %v", err)
		}
		if d.Metadata == nil || d.Metadata.IXML == nil || d.Metadata.IXML.Scene != "9" {
			t.Fatalf("iXML before fmt not decoded: %+v", d.Metadata)
		}
	})
}

// --- round trip and padding -------------------------------------------------

func TestIXMLEncoder_RoundTrip(t *testing.T) {
	originalPCM := []byte{
		0x01, 0x00, 0x02, 0x00,
		0xff, 0x7f, 0x00, 0x80,
		0x10, 0x20, 0x30, 0x40,
		0xaa, 0x55, 0x55, 0xaa,
	}

	t.Run("iXML set before writes lands before data, PCM written in parts", func(t *testing.T) {
		var out bytes.Buffer
		ws := &writeSeekerBuffer{buf: &out}
		e := NewEncoder(ws, 48000, 16, 2, 1)
		e.Metadata = &Metadata{IXML: expectedMultitrackIXML()}

		// write the PCM in several Write calls to exercise partial writes
		frame := func(vals ...int) *audio.IntBuffer {
			return &audio.IntBuffer{
				Format: &audio.Format{NumChannels: 2, SampleRate: 48000},
				Data:   vals,
			}
		}
		if err := e.Write(frame(1, 2)); err != nil {
			t.Fatal(err)
		}
		if err := e.Write(frame(32767, -32768)); err != nil {
			t.Fatal(err)
		}
		if err := e.Write(frame(0x2010, 0x4030)); err != nil {
			t.Fatal(err)
		}
		if err := e.Write(frame(0x55aa, -0x55ab)); err != nil {
			t.Fatal(err)
		}
		if err := e.Close(); err != nil {
			t.Fatalf("Close failed: %v", err)
		}

		data := out.Bytes()
		infos := walkChunks(t, data)
		pos := map[string]int{}
		for i, c := range infos {
			pos[c.id] = i
		}
		if !(pos["fmt "] < pos["iXML"] && pos["iXML"] < pos["data"]) {
			t.Fatalf("expected fmt < iXML < data, got %+v", pos)
		}
		for _, c := range infos {
			if c.id == "iXML" {
				if c.size%2 == 1 && (!c.hasPad || c.pad != 0) {
					t.Fatalf("odd iXML payload missing zero pad: %+v", c)
				}
				if c.size%2 == 0 && c.hasPad {
					t.Fatalf("even iXML payload must not be padded")
				}
			}
		}

		// PCM must be untouched by Close's header rewrites
		_, pcm, ok := findChunk(data, "data")
		if !ok {
			t.Fatal("data chunk missing")
		}
		if !bytes.Equal(pcm, originalPCM) {
			t.Fatalf("PCM changed after Close:\n got %v\nwant %v", pcm, originalPCM)
		}

		// read everything back
		d := NewDecoder(bytes.NewReader(data))
		if err := d.FwdToPCM(); err != nil {
			t.Fatalf("FwdToPCM failed: %v", err)
		}
		buf, err := d.FullPCMBuffer()
		if err != nil {
			t.Fatal(err)
		}
		wantSamples := []int{1, 2, 32767, -32768, 0x2010, 0x4030, 0x55aa, -0x55ab}
		if !intsEqual(buf.Data, wantSamples) {
			t.Fatalf("PCM samples = %v, want %v", buf.Data, wantSamples)
		}
		if d.Metadata == nil || d.Metadata.IXML == nil {
			t.Fatal("iXML metadata lost during round trip")
		}
		assertIXMLEqual(t, d.Metadata.IXML, expectedMultitrackIXML())
	})

	t.Run("iXML assigned after writes lands in the footer", func(t *testing.T) {
		var out bytes.Buffer
		ws := &writeSeekerBuffer{buf: &out}
		e := NewEncoder(ws, 48000, 16, 2, 1)
		buf := &audio.IntBuffer{
			Format: &audio.Format{NumChannels: 2, SampleRate: 48000},
			Data:   []int{1, 2, 32767, -32768, 0x2010, 0x4030, 0x55aa, -0x55ab},
		}
		if err := e.Write(buf); err != nil {
			t.Fatal(err)
		}
		// metadata only becomes available after the PCM was streamed
		e.Metadata = &Metadata{IXML: expectedMultitrackIXML()}
		if err := e.Close(); err != nil {
			t.Fatalf("Close failed: %v", err)
		}

		data := out.Bytes()
		infos := walkChunks(t, data)
		pos := map[string]int{}
		for i, c := range infos {
			pos[c.id] = i
		}
		if !(pos["fmt "] < pos["data"] && pos["data"] < pos["iXML"]) {
			t.Fatalf("expected fmt < data < iXML, got %+v", pos)
		}
		_, pcm, _ := findChunk(data, "data")
		if !bytes.Equal(pcm, originalPCM) {
			t.Fatalf("footer metadata write overwrote PCM:\n got %v\nwant %v", pcm, originalPCM)
		}

		d := NewDecoder(bytes.NewReader(data))
		d.ReadMetadata()
		if err := d.Err(); err != nil {
			t.Fatalf("ReadMetadata failed: %v", err)
		}
		if d.Metadata == nil || d.Metadata.IXML == nil {
			t.Fatal("footer iXML not decoded")
		}
		assertIXMLEqual(t, d.Metadata.IXML, expectedMultitrackIXML())
	})

	t.Run("both iXML parities are padded correctly by the encoder", func(t *testing.T) {
		render := func(scene string) []byte {
			var out bytes.Buffer
			ws := &writeSeekerBuffer{buf: &out}
			e := NewEncoder(ws, 44100, 16, 1, 1)
			e.Metadata = &Metadata{IXML: &IXMLMetadata{Scene: scene, Take: "1"}}
			if err := e.Write(&audio.IntBuffer{
				Format: &audio.Format{NumChannels: 1, SampleRate: 44100},
				Data:   []int{1, 2},
			}); err != nil {
				t.Fatal(err)
			}
			if err := e.Close(); err != nil {
				t.Fatal(err)
			}
			return out.Bytes()
		}
		seen := map[bool]bool{} // odd -> present
		for _, scene := range []string{"A", "AB"} {
			data := render(scene)
			info, payload, ok := findChunk(data, "iXML")
			if !ok {
				t.Fatal("iXML chunk missing")
			}
			odd := info.size%2 == 1
			seen[odd] = true
			if odd {
				if data[info.at+8+int(info.size)] != 0 {
					t.Fatalf("odd payload (%d bytes) not followed by zero pad", info.size)
				}
			}
			if int(info.size) != len(payload) {
				t.Fatalf("size field %d != payload length %d", info.size, len(payload))
			}
			// the padded file must still parse
			d := NewDecoder(bytes.NewReader(data))
			d.ReadMetadata()
			if err := d.Err(); err != nil {
				t.Fatalf("decoding parity variant failed: %v", err)
			}
			if d.Metadata == nil || d.Metadata.IXML == nil || d.Metadata.IXML.Scene != scene {
				t.Fatalf("scene %q did not round trip", scene)
			}
		}
		if !seen[true] || !seen[false] {
			t.Fatalf("test did not exercise both parities, saw %v", seen)
		}
	})
}

// --- error handling ---------------------------------------------------------

func TestIXMLParser_Errors(t *testing.T) {
	cases := []struct {
		name    string
		payload []byte
		want    error
	}{
		{"malformed truncated element", []byte(`<BWFXML><SCENE>1</SCENE>`), ErrMalformedIXML},
		{"not xml at all", []byte(`this is not xml`), ErrMalformedIXML},
		{"wrong root element", []byte(`<OTHER><SCENE>1</SCENE></OTHER>`), ErrMalformedIXML},
		{"empty payload", []byte(``), ErrMalformedIXML},
		{"only padding", []byte{0x00}, ErrMalformedIXML},
		{"duplicate conflicting scene",
			[]byte(`<BWFXML><SCENE>1</SCENE><SCENE>2</SCENE></BWFXML>`), ErrConflictingIXML},
		{"duplicate conflicting take",
			[]byte(`<BWFXML><TAKE>1</TAKE><TAKE>2</TAKE></BWFXML>`), ErrConflictingIXML},
		{"duplicate conflicting project",
			[]byte(`<BWFXML><PROJECT>a</PROJECT><PROJECT>b</PROJECT></BWFXML>`), ErrConflictingIXML},
		{"duplicate conflicting track name",
			[]byte(`<BWFXML><TRACK_LIST><TRACK><NAME>a</NAME><NAME>b</NAME></TRACK></TRACK_LIST></BWFXML>`),
			ErrConflictingIXML},
		{"track count mismatch",
			[]byte(`<BWFXML><TRACK_LIST><TRACK_COUNT>2</TRACK_COUNT><TRACK><NAME>a</NAME></TRACK></TRACK_LIST></BWFXML>`),
			ErrConflictingIXML},
		{"duplicate conflicting track list",
			[]byte(`<BWFXML><TRACK_LIST/><TRACK_LIST/></BWFXML>`), ErrConflictingIXML},
		{"invalid track count",
			[]byte(`<BWFXML><TRACK_LIST><TRACK_COUNT>abc</TRACK_COUNT></TRACK_LIST></BWFXML>`),
			ErrMalformedIXML},
		{"illegal rune",
			[]byte("<BWFXML><SCENE>bad\x00name</SCENE></BWFXML>"), ErrMalformedIXML},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseIXML(tc.payload)
			if !errors.Is(err, tc.want) {
				t.Fatalf("parseIXML() error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestIXMLParser_AcceptsLegalVariants(t *testing.T) {
	t.Run("single NUL pad byte on odd payload", func(t *testing.T) {
		payload := append([]byte(`<BWFXML><SCENE>7</SCENE></BWFXML>`), 0x00)
		ix, err := parseIXML(payload)
		if err != nil {
			t.Fatalf("odd payload with NUL pad rejected: %v", err)
		}
		if ix.Scene != "7" {
			t.Fatalf("scene = %q", ix.Scene)
		}
	})
	t.Run("duplicate identical fields are accepted", func(t *testing.T) {
		ix, err := parseIXML([]byte(`<BWFXML><SCENE>1</SCENE><SCENE>1</SCENE></BWFXML>`))
		if err != nil {
			t.Fatalf("duplicate identical field rejected: %v", err)
		}
		if ix.Scene != "1" {
			t.Fatalf("scene = %q", ix.Scene)
		}
	})
	t.Run("unknown nodes retained at root and in tracks", func(t *testing.T) {
		ix, err := parseIXML([]byte(`<BWFXML><PROJECT>P</PROJECT><SPEED><MASTER_SPEED>1.0</MASTER_SPEED></SPEED><TRACK_LIST><TRACK><NAME>n</NAME><CUSTOM_TAG>v</CUSTOM_TAG></TRACK></TRACK_LIST></BWFXML>`))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(ix.Extra["SPEED"], "MASTER_SPEED") {
			t.Fatalf("unknown root node not retained: %+v", ix.Extra)
		}
		if ix.Tracks[0].Extra["CUSTOM_TAG"] != "<CUSTOM_TAG>v</CUSTOM_TAG>" {
			t.Fatalf("unknown track node not retained: %+v", ix.Tracks[0].Extra)
		}
		// retained nodes must survive re-serialisation and re-parsing
		out, err := ix.render()
		if err != nil {
			t.Fatal(err)
		}
		reparsed, err := parseIXML(out)
		if err != nil {
			t.Fatalf("re-parsed rendered document: %v", err)
		}
		if !strings.Contains(reparsed.Extra["SPEED"], "MASTER_SPEED") {
			t.Fatalf("retained node lost on round trip: %+v", reparsed.Extra)
		}
		if reparsed.Tracks[0].Extra["CUSTOM_TAG"] != "<CUSTOM_TAG>v</CUSTOM_TAG>" {
			t.Fatalf("retained track node lost on round trip: %+v", reparsed.Tracks[0].Extra)
		}
	})

	t.Run("attributes of unknown nodes are retained", func(t *testing.T) {
		ix, err := parseIXML([]byte(`<BWFXML><USER role="recordist" id="42"><NOTE>hi</NOTE></USER></BWFXML>`))
		if err != nil {
			t.Fatal(err)
		}
		out, err := ix.render()
		if err != nil {
			t.Fatal(err)
		}
		rendered := string(out)
		if !strings.Contains(rendered, `role="recordist"`) || !strings.Contains(rendered, `id="42"`) {
			t.Fatalf("attributes lost on render: %s", rendered)
		}
		reparsed, err := parseIXML(out)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(reparsed.Extra["USER"], `role="recordist"`) {
			t.Fatalf("attributes lost on round trip: %q", reparsed.Extra["USER"])
		}
	})
}

func TestIXMLDecoder_ChunkLevelErrors(t *testing.T) {
	ixmlID := CIDiXML[:]

	t.Run("oversized chunk is rejected without reading it all", func(t *testing.T) {
		var body []byte
		body = append(body, "WAVE"...)
		body = append(body, rawFmtChunk(1, 16, 48000)...)
		// declare a far larger chunk than the file contains
		body = append(body, ixmlID...)
		body = append(body, leU32(MaxIXMLChunkSize+100)...)
		body = append(body, []byte("<BWFXML/>")...)
		riffBody := body
		file := append(append([]byte{}, "RIFF"...), leU32(uint32(len(riffBody)))...)
		file = append(file, riffBody...)

		d := NewDecoder(bytes.NewReader(file))
		d.ReadMetadata()
		if !errors.Is(d.Err(), ErrIXMLTooLarge) {
			t.Fatalf("error = %v, want ErrIXMLTooLarge", d.Err())
		}
	})

	t.Run("truncated chunk payload is an error", func(t *testing.T) {
		doc := []byte(`<BWFXML><SCENE>1</SCENE></BWFXML>`)
		var body []byte
		body = append(body, "WAVE"...)
		body = append(body, rawFmtChunk(1, 16, 48000)...)
		body = append(body, ixmlID...)
		body = append(body, leU32(uint32(len(doc)+50))...) // advertise more than available
		body = append(body, doc...)
		file := append(append([]byte{}, "RIFF"...), leU32(uint32(len(body)))...)
		file = append(file, body...)

		d := NewDecoder(bytes.NewReader(file))
		d.ReadMetadata()
		if !errors.Is(d.Err(), ErrTruncatedIXML) {
			t.Fatalf("error = %v, want ErrTruncatedIXML", d.Err())
		}
	})

	t.Run("malformed chunk before data fails FwdToPCM", func(t *testing.T) {
		data := buildRawWAV(
			rawFmtChunk(1, 16, 48000),
			rawChunk(CIDiXML, []byte(`<BWFXML><SCENE>1`)),
			rawChunk([4]byte{'d', 'a', 't', 'a'}, []byte{0, 0}),
		)
		d := NewDecoder(bytes.NewReader(data))
		err := d.FwdToPCM()
		if !errors.Is(err, ErrMalformedIXML) {
			t.Fatalf("FwdToPCM error = %v, want ErrMalformedIXML", err)
		}
	})

	t.Run("valid file without iXML keeps working", func(t *testing.T) {
		f, err := os.Open(filepath.Join("fixtures", "kick.wav"))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		d := NewDecoder(f)
		d.ReadMetadata()
		if err := d.Err(); err != nil {
			t.Fatalf("plain WAV unexpectedly failed: %v", err)
		}
		if d.Metadata != nil && d.Metadata.IXML != nil {
			t.Fatal("unexpected iXML metadata on a file without an iXML chunk")
		}
	})

	t.Run("malformed iXML before fmt fails FwdToPCM with a real error", func(t *testing.T) {
		var body []byte
		body = append(body, "WAVE"...)
		body = append(body, rawChunk(CIDiXML, []byte(`<BWFXML><SCENE>1`))...)
		body = append(body, rawFmtChunk(1, 16, 48000)...)
		body = append(body, rawChunk([4]byte{'d', 'a', 't', 'a'}, []byte{0, 0})...)
		file := append(append([]byte{}, "RIFF"...), leU32(uint32(len(body)))...)
		file = append(file, body...)

		d := NewDecoder(bytes.NewReader(file))
		err := d.FwdToPCM()
		if !errors.Is(err, ErrMalformedIXML) {
			t.Fatalf("FwdToPCM error = %v, want ErrMalformedIXML", err)
		}
	})

	t.Run("two conflicting iXML chunks are rejected", func(t *testing.T) {
		data := buildRawWAV(
			rawFmtChunk(1, 16, 48000),
			rawChunk(CIDiXML, []byte(`<BWFXML><SCENE>1</SCENE></BWFXML>`)),
			rawChunk(CIDiXML, []byte(`<BWFXML><SCENE>2</SCENE></BWFXML>`)),
			rawChunk([4]byte{'d', 'a', 't', 'a'}, []byte{0, 0}),
		)
		d := NewDecoder(bytes.NewReader(data))
		d.ReadMetadata()
		if !errors.Is(d.Err(), ErrConflictingIXML) {
			t.Fatalf("error = %v, want ErrConflictingIXML", d.Err())
		}
	})

	t.Run("two identical iXML chunks are accepted", func(t *testing.T) {
		doc := []byte(`<BWFXML><SCENE>5</SCENE></BWFXML>`)
		data := buildRawWAV(
			rawFmtChunk(1, 16, 48000),
			rawChunk(CIDiXML, doc),
			rawChunk(CIDiXML, doc),
			rawChunk([4]byte{'d', 'a', 't', 'a'}, []byte{0, 0}),
		)
		d := NewDecoder(bytes.NewReader(data))
		d.ReadMetadata()
		if err := d.Err(); err != nil {
			t.Fatalf("identical duplicates rejected: %v", err)
		}
		if d.Metadata == nil || d.Metadata.IXML == nil || d.Metadata.IXML.Scene != "5" {
			t.Fatalf("iXML not decoded: %+v", d.Metadata)
		}
	})

	t.Run("iXML before fmt survives a full ReadMetadata scan", func(t *testing.T) {
		doc := []byte(`<BWFXML><SCENE>8</SCENE><TAKE>2</TAKE></BWFXML>`)
		data := buildRawWAV(
			rawChunk(CIDiXML, doc),
			rawFmtChunk(1, 16, 48000),
			rawChunk([4]byte{'d', 'a', 't', 'a'}, []byte{0, 0}),
		)
		d := NewDecoder(bytes.NewReader(data))
		d.ReadMetadata()
		if err := d.Err(); err != nil {
			t.Fatalf("ReadMetadata failed: %v", err)
		}
		if d.Metadata == nil || d.Metadata.IXML == nil || d.Metadata.IXML.Scene != "8" {
			t.Fatalf("pre-fmt iXML lost: %+v", d.Metadata)
		}
	})

	t.Run("conflicting iXML with one copy before fmt", func(t *testing.T) {
		data := buildRawWAV(
			rawChunk(CIDiXML, []byte(`<BWFXML><SCENE>1</SCENE></BWFXML>`)),
			rawFmtChunk(1, 16, 48000),
			rawChunk(CIDiXML, []byte(`<BWFXML><SCENE>2</SCENE></BWFXML>`)),
			rawChunk([4]byte{'d', 'a', 't', 'a'}, []byte{0, 0}),
		)
		d := NewDecoder(bytes.NewReader(data))
		d.ReadMetadata()
		if !errors.Is(d.Err(), ErrConflictingIXML) {
			t.Fatalf("error = %v, want ErrConflictingIXML", d.Err())
		}
	})

	t.Run("ReadMetadata is idempotent and PCM stays readable", func(t *testing.T) {
		for _, p := range []string{
			filepath.Join("fixtures", "ixml-multitrack.wav"),
			filepath.Join("fixtures", "ixml-odd-footer.wav"),
		} {
			f, err := os.Open(p)
			if err != nil {
				t.Fatal(err)
			}
			d := NewDecoder(f)
			d.ReadMetadata()
			if err := d.Err(); err != nil {
				t.Fatalf("%s first ReadMetadata: %v", p, err)
			}
			first := d.Metadata.IXML.Scene
			d.ReadMetadata() // a second full scan must not report duplicates or errors
			if err := d.Err(); err != nil {
				t.Fatalf("%s second ReadMetadata: %v", p, err)
			}
			if d.Metadata.IXML.Scene != first {
				t.Fatalf("%s scene changed across scans: %q -> %q", p, first, d.Metadata.IXML.Scene)
			}
			// Rewind positions at the PCM chunk; read it directly afterwards.
			if err := d.Rewind(); err != nil {
				t.Fatalf("%s Rewind: %v", p, err)
			}
			buf, err := d.FullPCMBuffer()
			if err != nil {
				t.Fatalf("%s FullPCMBuffer after Rewind: %v", p, err)
			}
			if len(buf.Data) == 0 {
				t.Fatalf("%s no PCM samples read", p)
			}
			f.Close()
		}
	})
}

func TestIXMLEncoder_InvalidMetadataFails(t *testing.T) {
	t.Run("NUL rune in field prevents a successful close", func(t *testing.T) {
		var out bytes.Buffer
		ws := &writeSeekerBuffer{buf: &out}
		e := NewEncoder(ws, 44100, 16, 1, 1)
		e.Metadata = &Metadata{IXML: &IXMLMetadata{Scene: "bad\x00scene"}}
		writeErr := e.Write(&audio.IntBuffer{
			Format: &audio.Format{NumChannels: 1, SampleRate: 44100},
			Data:   []int{1, 2},
		})
		closeErr := e.Close()
		if writeErr == nil && closeErr == nil {
			t.Fatal("expected encoding invalid XML to fail, but Write and Close both succeeded")
		}
		// whatever was written must not decode as metadata-carrying success
		d := NewDecoder(bytes.NewReader(out.Bytes()))
		d.ReadMetadata()
		if d.Metadata != nil && d.Metadata.IXML != nil && d.Metadata.IXML.Scene != "" {
			t.Fatalf("invalid metadata was persisted: %+v", d.Metadata.IXML)
		}
	})

	t.Run("nil track entry fails", func(t *testing.T) {
		ix := &IXMLMetadata{Scene: "1", Tracks: []*IXMLTrack{nil}}
		if _, err := ix.render(); !errors.Is(err, ErrMalformedIXML) {
			t.Fatalf("render error = %v, want ErrMalformedIXML", err)
		}
	})
}

// --- helpers ----------------------------------------------------------------

type writeSeekerBuffer struct {
	buf *bytes.Buffer
	pos int64
}

func (w *writeSeekerBuffer) Write(p []byte) (int, error) {
	end := w.pos + int64(len(p))
	if end > int64(w.buf.Len()) {
		grow := make([]byte, end-int64(w.buf.Len()))
		w.buf.Write(grow)
	}
	copy(w.buf.Bytes()[w.pos:end], p)
	w.pos = end
	return len(p), nil
}

func (w *writeSeekerBuffer) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		w.pos = offset
	case io.SeekCurrent:
		w.pos += offset
	case io.SeekEnd:
		w.pos = int64(w.buf.Len()) + offset
	}
	if w.pos < 0 {
		return 0, errors.New("negative position")
	}
	return w.pos, nil
}

func intsEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
