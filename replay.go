package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

type replayOpts struct {
	apiKey  string
	baseURL string
	record  int
	noStream bool
}

func cmdReplay(args []string) {
	fs := flag.NewFlagSet("replay", flag.ExitOnError)
	apiKey := fs.String("api-key", "", "Target API key (required)")
	baseURL := fs.String("base-url", "https://api.anthropic.com", "Target API base URL")
	record := fs.Int("record", -1, "Record index to replay (0-based, default: last)")
	noStream := fs.Bool("no-stream", false, "Force non-streaming request")
	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}
	if fs.NArg() < 1 {
		fmt.Fprintf(os.Stderr, "Usage: ccreplay replay [flags] <input.jsonl>\n")
		os.Exit(1)
	}
	if *apiKey == "" {
		fmt.Fprintf(os.Stderr, "Error: -api-key is required\n")
		os.Exit(1)
	}

	input := fs.Arg(0)
	opts := replayOpts{
		apiKey:   *apiKey,
		baseURL:  strings.TrimRight(*baseURL, "/"),
		record:   *record,
		noStream: *noStream,
	}
	if err := runReplay(input, opts); err != nil {
		log.Fatal(err)
	}
}

func runReplay(input string, opts replayOpts) error {
	// Read JSONL
	rec, index, err := readRecord(input, opts.record)
	if err != nil {
		return err
	}

	fmt.Printf("Record #%d: %s %s\n", index, rec.Request.Method, rec.Request.URL)

	// Parse original request body
	var reqBody map[string]interface{}
	if err := json.Unmarshal(rec.Request.Body, &reqBody); err != nil {
		return fmt.Errorf("parse request body: %w", err)
	}

	origModel, _ := reqBody["model"].(string)
	fmt.Printf("Model:     %s\n\n", origModel)

	// Apply -no-stream
	if opts.noStream {
		reqBody["stream"] = false
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("marshal request body: %w", err)
	}

	// Parse original response
	origResult := parseOriginalResponse(rec)

	// Build and send replay request
	path := rec.Request.URL
	url := opts.baseURL + path

	httpReq, err := http.NewRequest(rec.Request.Method, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Api-Key", opts.apiKey)
	// Copy anthropic-version from original
	if v := rec.Request.Header["Anthropic-Version"]; len(v) > 0 {
		httpReq.Header.Set("Anthropic-Version", v[0])
	}
	if v := rec.Request.Header["Anthropic-Beta"]; len(v) > 0 {
		httpReq.Header.Set("Anthropic-Beta", v[0])
	}

	fmt.Printf("Sending request to %s ...\n", opts.baseURL)
	start := time.Now()
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()
	replayLatency := time.Since(start)

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("replay request failed with status %d: %s", resp.StatusCode, string(body))
	}

	// Parse replay response
	var replayResult responseResult
	isStream, _ := reqBody["stream"].(bool)
	if isStream {
		respBody, _ := io.ReadAll(resp.Body)
		replayResult = parseSSE(string(respBody))
	} else {
		respBody, _ := io.ReadAll(resp.Body)
		replayResult = parseJSONResponse(respBody)
	}

	// Output comparison
	printComparison(origResult, replayResult, replayLatency)
	return nil
}

// readRecord reads the specified record from the JSONL file.
func readRecord(input string, index int) (Record, int, error) {
	f, err := os.Open(input)
	if err != nil {
		return Record{}, 0, fmt.Errorf("open input: %w", err)
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
		return Record{}, 0, fmt.Errorf("read input: %w", err)
	}
	if len(records) == 0 {
		return Record{}, 0, fmt.Errorf("no records in %s", input)
	}

	// Resolve index
	if index < 0 {
		index = len(records) + index
	}
	if index < 0 || index >= len(records) {
		return Record{}, 0, fmt.Errorf("record index %d out of range (0-%d)", index, len(records)-1)
	}

	var rec Record
	if err := json.Unmarshal(records[index], &rec); err != nil {
		return Record{}, 0, fmt.Errorf("parse record %d: %w", index, err)
	}
	return rec, index, nil
}

type responseResult struct {
	Model       string
	InputTokens int
	OutputTokens int
	CacheRead   int
	CacheCreate int
	StopReason  string
	Content     string
}

// parseOriginalResponse parses the original response from the record.
func parseOriginalResponse(rec Record) responseResult {
	// The response body could be SSE string or JSON object
	var bodyStr string
	if err := json.Unmarshal(rec.Response.Body, &bodyStr); err == nil {
		// SSE string
		return parseSSE(bodyStr)
	}
	// Try as JSON object
	return parseJSONResponse(rec.Response.Body)
}

// parseSSE parses SSE event stream and extracts structured data.
func parseSSE(data string) responseResult {
	var result responseResult
	var contentParts []string
	var curToolName string
	var curToolInput strings.Builder

	scanner := bufio.NewScanner(strings.NewReader(data))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := line[6:]
		if payload == "[DONE]" {
			break
		}

		var event struct {
			Type         string `json:"type"`
			ContentBlock struct {
				Type string `json:"type"`
				Name string `json:"name"`
			} `json:"content_block"`
			Message struct {
				Model string `json:"model"`
				Usage struct {
					InputTokens              int `json:"input_tokens"`
					OutputTokens             int `json:"output_tokens"`
					CacheReadInputTokens     int `json:"cache_read_input_tokens"`
					CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
			Usage struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			continue
		}

		switch event.Type {
		case "message_start":
			result.Model = event.Message.Model
			result.InputTokens = event.Message.Usage.InputTokens
			result.OutputTokens = event.Message.Usage.OutputTokens
			result.CacheRead = event.Message.Usage.CacheReadInputTokens
			result.CacheCreate = event.Message.Usage.CacheCreationInputTokens
		case "content_block_start":
			if event.ContentBlock.Type == "tool_use" {
				curToolName = event.ContentBlock.Name
				curToolInput.Reset()
			}
		case "content_block_delta":
			switch event.Delta.Type {
			case "text_delta":
				contentParts = append(contentParts, event.Delta.Text)
			case "input_json_delta":
				curToolInput.WriteString(event.Delta.PartialJSON)
			}
		case "content_block_stop":
			if curToolName != "" {
				contentParts = append(contentParts, fmt.Sprintf("[tool_use: %s(%s)]", curToolName, curToolInput.String()))
				curToolName = ""
			}
		case "message_delta":
			result.StopReason = event.Delta.StopReason
			result.OutputTokens = event.Usage.OutputTokens
		}
	}

	result.Content = strings.Join(contentParts, "\n")
	return result
}

// parseJSONResponse parses a non-streaming JSON response.
func parseJSONResponse(data []byte) responseResult {
	var resp struct {
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return responseResult{}
	}

	var texts []string
	for _, c := range resp.Content {
		switch c.Type {
		case "text":
			texts = append(texts, c.Text)
		case "tool_use":
			texts = append(texts, fmt.Sprintf("[tool_use: %s(%s)]", c.Name, string(c.Input)))
		}
	}

	return responseResult{
		Model:        resp.Model,
		InputTokens:  resp.Usage.InputTokens,
		OutputTokens: resp.Usage.OutputTokens,
		CacheRead:    resp.Usage.CacheReadInputTokens,
		CacheCreate:  resp.Usage.CacheCreationInputTokens,
		StopReason:   resp.StopReason,
		Content:      strings.Join(texts, ""),
	}
}

func truncateText(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func printComparison(orig, replay responseResult, replayLatency time.Duration) {
	fmt.Printf("\n%-12s %-20s %-20s\n", "", "Original", "Replay")
	fmt.Printf("%-12s %-20s %-20s\n", "Model:", orig.Model, replay.Model)
	fmt.Printf("%-12s %-20s %-20s\n", "Latency:", "N/A", replayLatency.Round(time.Millisecond))
	fmt.Printf("%-12s %-20d %-20d\n", "Input:", orig.InputTokens, replay.InputTokens)
	fmt.Printf("%-12s %-20d %-20d\n", "Output:", orig.OutputTokens, replay.OutputTokens)
	fmt.Printf("%-12s %-20d %-20d\n", "Cache R:", orig.CacheRead, replay.CacheRead)
	fmt.Printf("%-12s %-20d %-20d\n", "Cache W:", orig.CacheCreate, replay.CacheCreate)
	fmt.Printf("%-12s %-20s %-20s\n", "Stop:", orig.StopReason, replay.StopReason)

	fmt.Printf("\n--- Original Response (truncated) ---\n%s\n", truncateText(orig.Content, 500))
	fmt.Printf("\n--- Replay Response (truncated) ---\n%s\n", truncateText(replay.Content, 500))
}
