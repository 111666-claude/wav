// This tool reads metadata from the passed wav file if available.
package main

import (
	"fmt"
	"log"
	"os"

	"github.com/go-audio/wav"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("You must pass the pass the path of the file to decode")
		os.Exit(1)
	}
	f, err := os.Open(os.Args[1])
	if err != nil {
		panic(err)
	}
	defer f.Close()
	dec := wav.NewDecoder(f)
	dec.ReadMetadata()
	if err := dec.Err(); err != nil {
		log.Fatal(err)
	}
	if dec.Metadata == nil && dec.Bext == nil {
		fmt.Println("No metadata present")
		return
	}
	if dec.Bext != nil {
		fmt.Println("Broadcast Audio Extension (bext):")
		fmt.Printf("Description: %s\n", dec.Bext.Description)
		fmt.Printf("Originator: %s\n", dec.Bext.Originator)
		fmt.Printf("OriginatorReference: %s\n", dec.Bext.OriginatorReference)
		fmt.Printf("OriginationDate: %s\n", dec.Bext.OriginationDate)
		fmt.Printf("OriginationTime: %s\n", dec.Bext.OriginationTime)
		fmt.Printf("TimeReference: %d\n", dec.Bext.TimeReference)
		fmt.Printf("Version: %d\n", dec.Bext.Version)
		fmt.Printf("CodingHistory: %s\n", dec.Bext.CodingHistory)
	}
	if dec.Metadata == nil {
		return
	}
	fmt.Printf("Artist: %s\n", dec.Metadata.Artist)
	fmt.Printf("Title: %s\n", dec.Metadata.Title)
	fmt.Printf("Comments: %s\n", dec.Metadata.Comments)
	fmt.Printf("Copyright: %s\n", dec.Metadata.Copyright)
	fmt.Printf("CreationDate: %s\n", dec.Metadata.CreationDate)
	fmt.Printf("Engineer: %s\n", dec.Metadata.Engineer)
	fmt.Printf("Technician: %s\n", dec.Metadata.Technician)
	fmt.Printf("Genre: %s\n", dec.Metadata.Genre)
	fmt.Printf("Keywords: %s\n", dec.Metadata.Keywords)
	fmt.Printf("Medium: %s\n", dec.Metadata.Medium)
	fmt.Printf("Product: %s\n", dec.Metadata.Product)
	fmt.Printf("Subject: %s\n", dec.Metadata.Subject)
	fmt.Printf("Software: %s\n", dec.Metadata.Software)
	fmt.Printf("Source: %s\n", dec.Metadata.Source)
	fmt.Printf("Location: %s\n", dec.Metadata.Location)
	fmt.Printf("TrackNbr: %s\n", dec.Metadata.TrackNbr)

	if dec.Metadata.SamplerInfo != nil {
		fmt.Println("Sample Info:")
		fmt.Printf("%+v\n", dec.Metadata.SamplerInfo)
		for i, l := range dec.Metadata.SamplerInfo.Loops {
			fmt.Printf("\tloop [%d]:\t%+v\n", i, l)
		}
	}
	for i, c := range dec.Metadata.CuePoints {
		fmt.Printf("\tcue point [%d]:\t%+v\n", i, c)
	}
}
