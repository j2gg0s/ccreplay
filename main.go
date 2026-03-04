package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: ccreplay <command> [flags]\n\nCommands:\n  proxy    Start HTTP reverse proxy\n  show     Generate HTML viewer from recorded JSONL\n  replay   Replay a recorded request to another endpoint\n")
		os.Exit(1)
	}

	switch os.Args[1] {
	case "proxy":
		cmdProxy(os.Args[2:])
	case "show":
		cmdShow(os.Args[2:])
	case "replay":
		cmdReplay(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", os.Args[1])
		os.Exit(1)
	}
}

func cmdShow(args []string) {
	fs := flag.NewFlagSet("show", flag.ExitOnError)
	output := fs.String("o", "", "Output HTML file (default: input with .html extension)")
	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}
	if fs.NArg() < 1 {
		fmt.Fprintf(os.Stderr, "Usage: ccreplay show [flags] <input.jsonl>\n")
		os.Exit(1)
	}
	input := fs.Arg(0)
	outFile := *output
	if outFile == "" {
		outFile = strings.TrimSuffix(input, ".jsonl") + ".html"
	}
	if err := runShow(input, outFile); err != nil {
		log.Fatal(err)
	}
}

func cmdProxy(args []string) {
	fs := flag.NewFlagSet("proxy", flag.ExitOnError)
	target := fs.String("target", "*.anthropic.com", "Target domain (suffix match, e.g. *.anthropic.com)")
	listen := fs.String("listen", ":9999", "Listen address")
	output := fs.String("output", ".", "Output directory for recorded traffic")
	truncate := fs.Bool("truncate", false, "Truncate recording file instead of appending")
	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}

	log.Printf("Starting proxy on %s -> %s", *listen, *target)
	if err := runProxy(*listen, *target, *output, *truncate); err != nil {
		log.Fatal(err)
	}
}
