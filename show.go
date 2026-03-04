package main

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

//go:embed viewer.html
var viewerHTML string

func runShow(input, output string) error {
	f, err := os.Open(input)
	if err != nil {
		return fmt.Errorf("open input: %w", err)
	}
	defer f.Close()

	var records []json.RawMessage
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		cp := make([]byte, len(line))
		copy(cp, line)
		records = append(records, json.RawMessage(cp))
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read input: %w", err)
	}

	allData, err := json.Marshal(records)
	if err != nil {
		return fmt.Errorf("marshal records: %w", err)
	}

	// Inject data into embedded-data placeholder
	const placeholder = `<script id="embedded-data" type="application/json">null</script>`
	replacement := `<script id="embedded-data" type="application/json">` + string(allData) + `</script>`
	html := strings.Replace(viewerHTML, placeholder, replacement, 1)

	// Set title
	html = strings.Replace(html, "<title>ccreplay</title>", "<title>"+input+" - ccreplay</title>", 1)

	out, err := os.Create(output)
	if err != nil {
		return fmt.Errorf("create output: %w", err)
	}
	defer out.Close()

	if _, err := out.WriteString(html); err != nil {
		return fmt.Errorf("write output: %w", err)
	}

	log.Printf("Written %s (%d turns)", output, len(records))

	// Open in browser
	switch runtime.GOOS {
	case "darwin":
		exec.Command("open", output).Start()
	case "linux":
		exec.Command("xdg-open", output).Start()
	}

	return nil
}
