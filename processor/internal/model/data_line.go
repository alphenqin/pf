package model

// DataLine represents a single line of data to be processed
type DataLine struct {
	Line      string
	IsHeader  bool
}