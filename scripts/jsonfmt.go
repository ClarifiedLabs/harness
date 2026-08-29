package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"harness/internal/modelcatalog"
)

func main() {
	catalog := flag.String("catalog", "", "prune to the harness modelsdev, codex, or codexrelease schema")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "usage: go run ./scripts/jsonfmt.go [-catalog modelsdev|codex|codexrelease] <input> <output>\n")
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

	out, err := formatCatalogJSON(data, *catalog)
	if err != nil {
		fmt.Fprintf(os.Stderr, "jsonfmt: format %s: %v\n", inPath, err)
		os.Exit(1)
	}

	if err := os.WriteFile(outPath, out, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "jsonfmt: write %s: %v\n", outPath, err)
		os.Exit(1)
	}
}

func formatCatalogJSON(data []byte, catalog string) ([]byte, error) {
	if catalog == "codexrelease" {
		version, err := modelcatalog.DecodeCodexReleaseVersion(data)
		if err != nil {
			return nil, err
		}
		return []byte(version + "\n"), nil
	}

	var dataToFormat []byte
	var err error
	switch catalog {
	case "":
		dataToFormat = data
	case "modelsdev":
		dataToFormat, err = modelcatalog.PruneModelsDevData(data)
	case "codex":
		dataToFormat, err = modelcatalog.PruneCodexModelsData(data)
	default:
		return nil, fmt.Errorf("unknown catalog %q", catalog)
	}
	if err != nil {
		return nil, err
	}

	var out bytes.Buffer
	if err := json.Indent(&out, bytes.TrimSpace(dataToFormat), "", "  "); err != nil {
		return nil, err
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}
