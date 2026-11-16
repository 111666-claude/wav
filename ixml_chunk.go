package wav

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/go-audio/riff"
)

// CIDiXML is the chunk ID for an iXML chunk.
// iXML is a RIFF chunk used by location sound recorders to carry production
// metadata such as the project, scene, take and track names.
// See https://wavref.til.cafe/chunk/ixml/ and http://www.ixml.info/
var CIDiXML = [4]byte{'i', 'X', 'M', 'L'}

// MaxIXMLChunkSize is the largest iXML payload (not counting the 8 byte chunk
// header) this package is willing to read. Real world iXML chunks are only a
// few kilobytes long; accepting arbitrarily large chunks would allow a
// corrupt or malicious file to exhaust the available memory.
const MaxIXMLChunkSize = 16 * 1024 * 1024

var (
	// ErrMalformedIXML indicates that an iXML chunk could not be parsed as
	// valid XML.
	ErrMalformedIXML = errors.New("malformed iXML chunk")
	// ErrConflictingIXML indicates that an iXML chunk contains several
	// definitions of the same field with different values.
	ErrConflictingIXML = errors.New("conflicting iXML fields")
	// ErrIXMLTooLarge indicates that an iXML chunk declares a size bigger than
	// MaxIXMLChunkSize.
	ErrIXMLTooLarge = errors.New("iXML chunk is too large")
	// ErrTruncatedIXML indicates that an iXML chunk does not contain the full
	// amount of bytes advertised in its RIFF chunk header.
	ErrTruncatedIXML = errors.New("truncated iXML chunk")
)

// IXMLTrack represents a single TRACK entry of an iXML TRACK_LIST.
type IXMLTrack struct {
	// ChannelIndex is the recorder input/source number, counted from 1.
	// Armed tracks do not have to be contiguous. 0 means the field was absent.
	ChannelIndex int
	// InterleaveIndex is the position of the track inside the file, counted
	// from 1. 0 means the field was absent.
	InterleaveIndex int
	// Name is the human readable label of the track, e.g. "Boom 1".
	Name string
	// Function describes the purpose of the track using the iXML FUNCTION
	// dictionary, e.g. "LEFT". It may be empty.
	Function string
	// Extra holds the unrecognised child elements of the TRACK entry, keyed by
	// tag name. Their raw inner XML is preserved so unknown but legal nodes
	// survive a read/write round trip.
	Extra map[string]string
}

// IXMLMetadata is the structured representation of an iXML chunk.
// The documented production sound fields are exposed directly; any other
// legal XML node found in the BWFXML document is kept as raw XML in Extra so
// it is not lost when the metadata is written back.
type IXMLMetadata struct {
	// Version is the IXML_VERSION of the document, e.g. "2.0".
	Version string
	// Project is the PROJECT (production / picture title).
	Project string
	// Scene is the SCENE / slate identifier, e.g. "21A".
	Scene string
	// Take is the TAKE number within the scene.
	Take string
	// Tracks is the list of TRACK entries found in TRACK_LIST.
	Tracks []*IXMLTrack

	// Extra holds the unrecognised top level children of BWFXML, keyed by tag
	// name, each value being the raw inner XML of the node. Repeated unknown
	// tags with the same name are concatenated with their wrapping tags so no
	// data is dropped.
	Extra map[string]string

	// root keeps the parsed document so unknown nodes can be re-emitted
	// alongside the structured fields when the chunk is written back.
	root *ixmlNode
	// canonical is the compact XML rendering of root, used to detect two iXML
	// chunks carrying conflicting content in the same file.
	canonical string
}

// ixmlNode is a minimal DOM node used to retain the full structure of an iXML
// document, including nodes this package does not model explicitly.
type ixmlNode struct {
	Name     string
	Attrs    []xml.Attr
	Text     string
	Children []*ixmlNode
}

// iXML scalar fields modelled at the top level of BWFXML.
var ixmlScalarFields = map[string]bool{
	"IXML_VERSION": true,
	"PROJECT":      true,
	"SCENE":        true,
	"TAKE":         true,
}

func (n *ixmlNode) findChild(name string) *ixmlNode {
	for _, c := range n.Children {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// DecodeIXMLChunk decodes an iXML chunk and stores the result in d.Metadata.IXML.
func DecodeIXMLChunk(d *Decoder, ch *riff.Chunk) error {
	if ch == nil {
		return fmt.Errorf("can't decode a nil chunk")
	}
	if d == nil {
		return fmt.Errorf("nil decoder")
	}
	if ch.ID != CIDiXML {
		ch.Drain()
		return nil
	}
	ix, err := d.readIXMLPayload(ch)
	if err != nil {
		return err
	}
	if d.Metadata == nil {
		d.Metadata = &Metadata{}
	}
	if d.ixmlSeen {
		// more than one iXML chunk is only legal when they carry the same data
		if d.ixmlCanonical != ix.canonical {
			return fmt.Errorf("%w: file contains two iXML chunks with different content",
				ErrConflictingIXML)
		}
		// identical duplicate: keep the first one
		return nil
	}
	d.ixmlSeen = true
	d.ixmlCanonical = ix.canonical
	d.Metadata.IXML = ix
	return nil
}

// readIXMLPayload reads and parses the payload of an open iXML chunk.
func (d *Decoder) readIXMLPayload(ch *riff.Chunk) (*IXMLMetadata, error) {
	// ch.Size includes the RIFF padding byte; the size field of the chunk only
	// counts the XML payload. Guard against absurd sizes before allocating any
	// memory so a corrupt file cannot trigger a huge allocation.
	if ch.Size > MaxIXMLChunkSize+1 {
		ch.Drain()
		return nil, fmt.Errorf("%w: declared size of %d bytes exceeds the %d byte limit",
			ErrIXMLTooLarge, ch.Size, MaxIXMLChunkSize)
	}
	buf := make([]byte, ch.Size)
	n, err := io.ReadFull(ch.R, buf)
	if err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil, fmt.Errorf("%w: read %d of %d advertised bytes: %w", ErrTruncatedIXML, n, ch.Size, err)
		}
		return nil, fmt.Errorf("failed to read the iXML chunk: %w", err)
	}
	// io.ReadFull consumed the whole (padded) payload through the limit reader
	// but the chunk's own position counter was not advanced. Mark it as fully
	// read so a subsequent Drain is a no-op rather than skipping the next chunk.
	ch.Pos = ch.Size
	return parseIXML(buf)
}

// parseIXML parses the raw bytes of an iXML chunk payload. A single trailing
// NUL pad byte (RIFF word alignment) is tolerated.
func parseIXML(payload []byte) (*IXMLMetadata, error) {
	// RIFF chunks are word aligned: an odd sized payload is followed by one
	// padding byte which is part of the chunk the reader hands us but not part
	// of the XML document.
	xmlBytes := payload
	if len(xmlBytes) > 0 && xmlBytes[len(xmlBytes)-1] == 0 {
		xmlBytes = xmlBytes[:len(xmlBytes)-1]
	}
	if len(bytes.TrimSpace(xmlBytes)) == 0 {
		return nil, fmt.Errorf("%w: empty payload", ErrMalformedIXML)
	}

	dec := xml.NewDecoder(bytes.NewReader(xmlBytes))
	dec.Strict = true

	root, err := buildIXMLTree(dec)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformedIXML, err)
	}
	if root == nil {
		return nil, fmt.Errorf("%w: no XML root element", ErrMalformedIXML)
	}
	if root.Name != "BWFXML" {
		return nil, fmt.Errorf("%w: expected a BWFXML root element but got %q", ErrMalformedIXML, root.Name)
	}

	ix := &IXMLMetadata{root: root, Extra: map[string]string{}}

	// scalar, single-valued production fields
	scalars := []struct {
		tag string
		dst *string
	}{
		{"IXML_VERSION", &ix.Version},
		{"PROJECT", &ix.Project},
		{"SCENE", &ix.Scene},
		{"TAKE", &ix.Take},
	}
	for _, f := range scalars {
		var seen bool
		for _, c := range root.Children {
			if c.Name != f.tag {
				continue
			}
			val := strings.TrimSpace(c.Text)
			if seen && *f.dst != val {
				return nil, fmt.Errorf("%w: repeated <%s> with different values %q and %q",
					ErrConflictingIXML, f.tag, *f.dst, val)
			}
			*f.dst = val
			seen = true
		}
	}

	// track list
	var trackList *ixmlNode
	for _, c := range root.Children {
		if c.Name == "TRACK_LIST" {
			if trackList != nil {
				return nil, fmt.Errorf("%w: repeated <TRACK_LIST>", ErrConflictingIXML)
			}
			trackList = c
		}
	}
	if trackList != nil {
		var countSeen bool
		var declaredCount int
		for _, c := range trackList.Children {
			switch c.Name {
			case "TRACK_COUNT":
				val := strings.TrimSpace(c.Text)
				n, err := strconv.Atoi(val)
				if err != nil {
					return nil, fmt.Errorf("%w: invalid TRACK_COUNT %q: %v", ErrMalformedIXML, val, err)
				}
				if countSeen && declaredCount != n {
					return nil, fmt.Errorf("%w: conflicting TRACK_COUNT values %d and %d",
						ErrConflictingIXML, declaredCount, n)
				}
				declaredCount, countSeen = n, true
			case "TRACK":
				tr, err := parseIXMLTrack(c)
				if err != nil {
					return nil, err
				}
				ix.Tracks = append(ix.Tracks, tr)
			default:
				// unknown but legal TRACK_LIST child: keep on the node as-is
			}
		}
		if countSeen && declaredCount != len(ix.Tracks) {
			return nil, fmt.Errorf("%w: TRACK_COUNT is %d but %d TRACK entries were found",
				ErrConflictingIXML, declaredCount, len(ix.Tracks))
		}
	}

	// retain every unknown top level node as raw XML
	for _, c := range root.Children {
		if ixmlScalarFields[c.Name] || c.Name == "TRACK_LIST" {
			continue
		}
		raw, err := marshalIXMLNode(c)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrMalformedIXML, err)
		}
		if existing, ok := ix.Extra[c.Name]; ok {
			ix.Extra[c.Name] = existing + string(raw)
		} else {
			ix.Extra[c.Name] = string(raw)
		}
	}
	if len(ix.Extra) == 0 {
		ix.Extra = nil
	}

	canonical, err := marshalIXMLTreeCompact(root)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformedIXML, err)
	}
	ix.canonical = canonical

	return ix, nil
}

// marshalIXMLTreeCompact renders a node without indentation so two documents
// differing only in whitespace compare as equal.
func marshalIXMLTreeCompact(n *ixmlNode) (string, error) {
	var buf bytes.Buffer
	enc := xml.NewEncoder(&buf)
	if err := encodeIXMLNode(enc, n); err != nil {
		return "", err
	}
	if err := enc.Flush(); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func parseIXMLTrack(node *ixmlNode) (*IXMLTrack, error) {
	tr := &IXMLTrack{Extra: map[string]string{}}
	intFields := []struct {
		tag string
		dst *int
	}{
		{"CHANNEL_INDEX", &tr.ChannelIndex},
		{"INTERLEAVE_INDEX", &tr.InterleaveIndex},
	}
	for _, f := range intFields {
		var seen bool
		for _, c := range node.Children {
			if c.Name != f.tag {
				continue
			}
			val := strings.TrimSpace(c.Text)
			n, err := strconv.Atoi(val)
			if err != nil {
				return nil, fmt.Errorf("%w: invalid %s %q: %v", ErrMalformedIXML, f.tag, val, err)
			}
			if seen && *f.dst != n {
				return nil, fmt.Errorf("%w: conflicting %s values %d and %d",
					ErrConflictingIXML, f.tag, *f.dst, n)
			}
			*f.dst, seen = n, true
		}
	}
	var nameSeen bool
	for _, c := range node.Children {
		switch c.Name {
		case "NAME":
			val := strings.TrimSpace(c.Text)
			if nameSeen && tr.Name != val {
				return nil, fmt.Errorf("%w: conflicting track NAME values %q and %q",
					ErrConflictingIXML, tr.Name, val)
			}
			tr.Name, nameSeen = val, true
		case "FUNCTION":
			val := strings.TrimSpace(c.Text)
			if tr.Function != "" && tr.Function != val {
				return nil, fmt.Errorf("%w: conflicting track FUNCTION values %q and %q",
					ErrConflictingIXML, tr.Function, val)
			}
			tr.Function = val
		case "CHANNEL_INDEX", "INTERLEAVE_INDEX":
			continue
		default:
			raw, err := marshalIXMLNode(c)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", ErrMalformedIXML, err)
			}
			if existing, ok := tr.Extra[c.Name]; ok {
				tr.Extra[c.Name] = existing + string(raw)
			} else {
				tr.Extra[c.Name] = string(raw)
			}
		}
	}
	if len(tr.Extra) == 0 {
		tr.Extra = nil
	}
	return tr, nil
}

// buildIXMLTree consumes the tokens of a strict XML decoder and builds a tree
// of ixmlNode values. Chardata outside of elements is rejected so documents
// cannot smuggle content past the parser.
func buildIXMLTree(dec *xml.Decoder) (*ixmlNode, error) {
	var root *ixmlNode
	stack := []*ixmlNode{}
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			node := &ixmlNode{Name: t.Name.Local, Attrs: append([]xml.Attr{}, t.Attr...)}
			if len(stack) > 0 {
				parent := stack[len(stack)-1]
				parent.Children = append(parent.Children, node)
			} else if root == nil {
				root = node
			} else {
				return nil, fmt.Errorf("multiple root elements")
			}
			stack = append(stack, node)
		case xml.EndElement:
			if len(stack) == 0 || stack[len(stack)-1].Name != t.Name.Local {
				return nil, fmt.Errorf("unexpected end element </%s>", t.Name.Local)
			}
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if len(stack) == 0 {
				if len(bytes.TrimSpace(t)) > 0 {
					return nil, fmt.Errorf("unexpected text outside of the root element")
				}
				continue
			}
			stack[len(stack)-1].Text += string(t)
		case xml.ProcInst, xml.Directive, xml.Comment:
			// the XML declaration and comments are legal but not modelled
		}
	}
	if len(stack) != 0 {
		return nil, io.ErrUnexpectedEOF
	}
	return root, nil
}

// marshalIXMLNode renders a node back to compact XML.
func marshalIXMLNode(n *ixmlNode) ([]byte, error) {
	var buf bytes.Buffer
	enc := xml.NewEncoder(&buf)
	if err := encodeIXMLNode(enc, n); err != nil {
		return nil, err
	}
	if err := enc.Flush(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func encodeIXMLNode(enc *xml.Encoder, n *ixmlNode) error {
	start := xml.StartElement{Name: xml.Name{Local: n.Name}, Attr: n.Attrs}
	if len(n.Children) == 0 {
		if err := enc.EncodeToken(start); err != nil {
			return err
		}
		if n.Text != "" {
			if err := enc.EncodeToken(xml.CharData([]byte(n.Text))); err != nil {
				return err
			}
		}
		return enc.EncodeToken(xml.EndElement{Name: start.Name})
	}
	if err := enc.EncodeToken(start); err != nil {
		return err
	}
	if n.Text != "" {
		if err := enc.EncodeToken(xml.CharData([]byte(n.Text))); err != nil {
			return err
		}
	}
	for _, c := range n.Children {
		if err := encodeIXMLNode(enc, c); err != nil {
			return err
		}
	}
	return enc.EncodeToken(xml.EndElement{Name: start.Name})
}

// render serialises the metadata into an iXML XML document. The structured
// fields are synchronised into the retained DOM first so unknown nodes are
// preserved while known fields always reflect the Go values.
func (ix *IXMLMetadata) render() ([]byte, error) {
	if ix == nil {
		return nil, nil
	}
	root := ix.root
	if root == nil {
		root = &ixmlNode{Name: "BWFXML"}
	} else {
		// never mutate the document that was decoded
		root = cloneIXMLTree(root)
	}

	setScalar := func(tag, value string) {
		if value == "" {
			return
		}
		if node := root.findChild(tag); node != nil {
			node.Text = value
			node.Children = nil
			return
		}
		root.Children = append(root.Children, &ixmlNode{Name: tag, Text: value})
	}
	setScalar("IXML_VERSION", ix.Version)
	setScalar("PROJECT", ix.Project)
	setScalar("SCENE", ix.Scene)
	setScalar("TAKE", ix.Take)

	if len(ix.Tracks) > 0 {
		tl := root.findChild("TRACK_LIST")
		if tl == nil {
			tl = &ixmlNode{Name: "TRACK_LIST"}
			root.Children = append(root.Children, tl)
		} else {
			tl = cloneIXMLTree(tl)
			// replace the node in the parent copy
			for i, c := range root.Children {
				if c.Name == "TRACK_LIST" {
					root.Children[i] = tl
					break
				}
			}
		}
		// drop the previously modelled entries, keep unknown children
		kept := make([]*ixmlNode, 0, len(tl.Children))
		for _, c := range tl.Children {
			if c.Name != "TRACK" && c.Name != "TRACK_COUNT" {
				kept = append(kept, c)
			}
		}
		tl.Children = kept
		tl.Children = append(tl.Children, &ixmlNode{Name: "TRACK_COUNT", Text: strconv.Itoa(len(ix.Tracks))})
		for _, tr := range ix.Tracks {
			if tr == nil {
				return nil, fmt.Errorf("%w: nil track in TRACK_LIST", ErrMalformedIXML)
			}
			node := &ixmlNode{Name: "TRACK"}
			if tr.ChannelIndex > 0 {
				node.Children = append(node.Children, &ixmlNode{Name: "CHANNEL_INDEX", Text: strconv.Itoa(tr.ChannelIndex)})
			}
			if tr.InterleaveIndex > 0 {
				node.Children = append(node.Children, &ixmlNode{Name: "INTERLEAVE_INDEX", Text: strconv.Itoa(tr.InterleaveIndex)})
			}
			if tr.Name != "" {
				node.Children = append(node.Children, &ixmlNode{Name: "NAME", Text: tr.Name})
			}
			if tr.Function != "" {
				node.Children = append(node.Children, &ixmlNode{Name: "FUNCTION", Text: tr.Function})
			}
			for tag, raw := range tr.Extra {
				extra, err := parseRawFragment(tag, raw)
				if err != nil {
					return nil, err
				}
				node.Children = append(node.Children, extra...)
			}
			tl.Children = append(tl.Children, node)
		}
	}

	// fold back extra top level nodes that were not present in the DOM
	for tag, raw := range ix.Extra {
		if ixmlScalarFields[tag] || tag == "TRACK_LIST" || root.findChild(tag) != nil {
			continue
		}
		nodes, err := parseRawFragment(tag, raw)
		if err != nil {
			return nil, err
		}
		root.Children = append(root.Children, nodes...)
	}

	if err := validateIXMLTree(root); err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	enc := xml.NewEncoder(&buf)
	enc.Indent("", "  ")
	if err := encodeIXMLNode(enc, root); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformedIXML, err)
	}
	if err := enc.Flush(); err != nil {
		return nil, err
	}
	out := buf.Bytes()
	if len(out) > MaxIXMLChunkSize {
		return nil, fmt.Errorf("%w: rendered document is %d bytes long", ErrIXMLTooLarge, len(out))
	}
	return out, nil
}

// parseRawFragment parses one or more top level nodes stored as raw XML.
func parseRawFragment(wrappingTag, raw string) ([]*ixmlNode, error) {
	dec := xml.NewDecoder(strings.NewReader(raw))
	dec.Strict = true
	var nodes []*ixmlNode
	for {
		node, err := buildIXMLTree(dec)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: invalid retained <%s> XML: %v", ErrMalformedIXML, wrappingTag, err)
		}
		if node == nil {
			break
		}
		nodes = append(nodes, node)
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("%w: empty retained <%s> XML", ErrMalformedIXML, wrappingTag)
	}
	return nodes, nil
}

func cloneIXMLTree(n *ixmlNode) *ixmlNode {
	if n == nil {
		return nil
	}
	cp := &ixmlNode{Name: n.Name, Text: n.Text, Attrs: append([]xml.Attr{}, n.Attrs...)}
	for _, c := range n.Children {
		cp.Children = append(cp.Children, cloneIXMLTree(c))
	}
	return cp
}

// validateIXMLTree makes sure no illegal XML rune (encoding/xml happily emits
// some of them) is about to be written into the chunk.
func validateIXMLTree(n *ixmlNode) error {
	if strings.ContainsRune(n.Name, 0) {
		return fmt.Errorf("%w: NUL byte in element name %q", ErrMalformedIXML, n.Name)
	}
	if strings.ContainsRune(n.Text, 0) {
		return fmt.Errorf("%w: NUL byte in text of <%s>", ErrMalformedIXML, n.Name)
	}
	for _, c := range n.Children {
		if err := validateIXMLTree(c); err != nil {
			return err
		}
	}
	return nil
}
