package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
)

func main() {
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "usage: go run ./scripts/jsonfmt.go <input> <output>\n")
	}
	flag.Parse()
	if flag.NArg() != 2 {
		flag.Usage()
		os.Exit(2)
	}

	inPath := flag.Arg(0)
	outPath := flag.Arg(1)
	data, err := os.ReadFile(inPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "jsonfmt: read %s: %v\n", inPath, err)
		os.Exit(1)
	}

	var out bytes.Buffer
	if err := json.Indent(&out, data, "", "  "); err != nil {
		fmt.Fprintf(os.Stderr, "jsonfmt: format %s: %v\n", inPath, err)
		os.Exit(1)
	}
	out.WriteByte('\n')

	if err := os.WriteFile(outPath, out.Bytes(), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "jsonfmt: write %s: %v\n", outPath, err)
		os.Exit(1)
	}
}
