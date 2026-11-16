# wav codec

[![GoDoc](https://godoc.org/github.com/go-audio/wav?status.svg)](https://godoc.org/github.com/go-audio/wav)
[![Go Report Card](https://goreportcard.com/badge/github.com/go-audio/wav)](https://goreportcard.com/report/github.com/go-audio/wav)
[![Coverage Status](https://codecov.io/gh/go-audio/wav/graph/badge.svg)](https://codecov.io/gh/go-audio/wav)
[![Build Status](https://travis-ci.org/go-audio/wav.svg)](https://travis-ci.org/go-audio/wav)

Wav audio file encoder and decoder. See [GoDoc](https://godoc.org/github.com/go-audio/wav) for more details.

## iXML production metadata

The optional [`iXML`](https://wavref.til.cafe/chunk/ixml/) chunk carries
location-sound metadata (project, scene, take and per-track names). It is
exposed through `Metadata.IXML`.

Reading — iXML chunks are found whether they sit before or after the `data`
chunk:

```go
d := wav.NewDecoder(f)
d.ReadMetadata()
if err := d.Err(); err != nil {
    log.Fatal(err)
}
if d.Metadata != nil && d.Metadata.IXML != nil {
    ix := d.Metadata.IXML
    fmt.Println(ix.Project, ix.Scene, ix.Take)
    for _, tr := range ix.Tracks {
        fmt.Println(tr.InterleaveIndex, tr.Name)
    }
}
```

Writing — set the metadata on the encoder. Metadata present before the first
`Write` is emitted next to the `fmt` chunk; metadata assigned afterwards is
written as a footer at `Close()`. Odd-sized payloads are word-aligned with a
single zero padding byte:

```go
e := wav.NewEncoder(out, 48000, 16, 2, 1)
e.Metadata = &wav.Metadata{IXML: &wav.IXMLMetadata{
    Project: "Field Recordings",
    Scene:   "12A",
    Take:    "3",
    Tracks: []*wav.IXMLTrack{
        {ChannelIndex: 1, InterleaveIndex: 1, Name: "Boom"},
        {ChannelIndex: 2, InterleaveIndex: 2, Name: "Lav 1"},
    },
}}
```

Malformed XML, conflicting duplicate fields, oversized or truncated chunks
return an error rather than producing a superficially valid file. Unrecognised
but legal XML nodes are retained and survive a read/write round trip.
