package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type contextKey int

const reqBodyKey contextKey = 0

// Record represents a single request/response pair.
type Record struct {
	Timestamp string        `json:"timestamp"`
	Request   RecordRequest `json:"request"`
	Response  RecordResponse `json:"response"`
}

type RecordRequest struct {
	Method string              `json:"method"`
	URL    string              `json:"url"`
	Header map[string][]string `json:"header"`
	Body   json.RawMessage     `json:"body"`
}

type RecordResponse struct {
	Status int                 `json:"status"`
	Header map[string][]string `json:"header"`
	Body   json.RawMessage     `json:"body"`
}

// recorder manages writing records to a JSONL file.
type recorder struct {
	mu   sync.Mutex
	file *os.File
}

func newRecorder(outputDir string, truncate bool) (*recorder, error) {
	path := filepath.Join(outputDir, "ccreplay.jsonl")

	flag := os.O_CREATE | os.O_WRONLY
	if truncate {
		flag |= os.O_TRUNC
	} else {
		flag |= os.O_APPEND
	}
	f, err := os.OpenFile(path, flag, 0644)
	if err != nil {
		return nil, fmt.Errorf("create recording file: %w", err)
	}
	log.Printf("Recording to %s (truncate=%v)", path, truncate)
	return &recorder{file: f}, nil
}

func (r *recorder) write(rec Record) {
	data, err := json.Marshal(rec)
	if err != nil {
		log.Printf("marshal record: %v", err)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.file.Write(append(data, '\n'))
}

// recordingBody wraps a response body to capture its content.
type recordingBody struct {
	orig   io.ReadCloser
	tee    io.Reader
	buf    bytes.Buffer
	req    *http.Request
	resp   *http.Response
	rec    *recorder
}

func (rb *recordingBody) Read(p []byte) (int, error) {
	return rb.tee.Read(p)
}

func (rb *recordingBody) Close() error {
	err := rb.orig.Close()

	reqBody, _ := rb.req.Context().Value(reqBodyKey).([]byte)

	rb.rec.write(Record{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Request: RecordRequest{
			Method: rb.req.Method,
			URL:    rb.req.URL.RequestURI(),
			Header: rb.req.Header,
			Body:   jsonBody(reqBody),
		},
		Response: RecordResponse{
			Status: rb.resp.StatusCode,
			Header: rb.resp.Header,
			Body:   jsonBody(rb.buf.Bytes()),
		},
	})

	return err
}

// jsonBody returns raw bytes as json.RawMessage if valid JSON,
// otherwise encodes as a JSON string.
func jsonBody(b []byte) json.RawMessage {
	if len(b) == 0 {
		return json.RawMessage("null")
	}
	if json.Valid(b) {
		return json.RawMessage(b)
	}
	encoded, _ := json.Marshal(string(b))
	return json.RawMessage(encoded)
}

// matchDomain checks if host matches the target pattern.
// Pattern "*.example.com" matches "example.com" and any subdomain like "api.example.com".
func matchDomain(host, pattern string) bool {
	if i := strings.LastIndex(host, ":"); i != -1 {
		host = host[:i]
	}

	if !strings.HasPrefix(pattern, "*.") {
		return strings.EqualFold(host, pattern)
	}

	base := pattern[2:]
	if strings.EqualFold(host, base) {
		return true
	}
	return strings.HasSuffix(strings.ToLower(host), "."+strings.ToLower(base))
}

func runProxy(listen, target, outputDir string, truncate bool) error {
	rec, err := newRecorder(outputDir, truncate)
	if err != nil {
		return err
	}
	defer rec.file.Close()

	upstreamHost := target
	if strings.HasPrefix(target, "*.") {
		upstreamHost = "api" + target[1:]
	}

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = "https"
			req.URL.Host = upstreamHost
			req.Host = upstreamHost
			// Remove Accept-Encoding so that Go's Transport handles decompression
			// transparently, ensuring we record uncompressed body.
			req.Header.Del("Accept-Encoding")
		},
		ModifyResponse: func(resp *http.Response) error {
			rb := &recordingBody{
				orig: resp.Body,
				req:  resp.Request,
				resp: resp,
				rec:  rec,
			}
			rb.tee = io.TeeReader(rb.orig, &rb.buf)
			resp.Body = rb
			return nil
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("proxy error: %s %s: %v", r.Method, r.URL.Path, err)
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprintf(w, "proxy error: %v", err)
		},
	}

	// Start viewer on a separate port
	go startViewer(listen, rec)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if host == "" {
			host = r.URL.Host
		}
		if !matchDomain(host, target) && !isLocalHost(host) {
			log.Printf("rejected request with Host: %s (target: %s)", host, target)
			http.Error(w, "forbidden: host not allowed", http.StatusForbidden)
			return
		}

		// Capture request body
		if r.Body != nil {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				log.Printf("read request body: %v", err)
				http.Error(w, "failed to read request body", http.StatusInternalServerError)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			r = r.WithContext(context.WithValue(r.Context(), reqBodyKey, body))
		}

		log.Printf("%s %s", r.Method, r.URL.Path)
		proxy.ServeHTTP(w, r)
	})

	server := &http.Server{
		Addr:         listen,
		Handler:      handler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 5 * time.Minute,
	}

	return server.ListenAndServe()
}

// isLocalHost returns true if the host points to localhost.
func isLocalHost(host string) bool {
	if i := strings.LastIndex(host, ":"); i != -1 {
		host = host[:i]
	}
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}

// startViewer starts the viewer HTTP server on proxyAddr port + 1.
func startViewer(proxyAddr string, rec *recorder) {
	// Derive viewer port from proxy listen address
	viewerPort := 10000
	if i := strings.LastIndex(proxyAddr, ":"); i != -1 {
		var port int
		if _, err := fmt.Sscanf(proxyAddr[i+1:], "%d", &port); err == nil {
			viewerPort = port + 1
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/_/api/records", func(w http.ResponseWriter, r *http.Request) {
		serveRecordsAPI(w, rec)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(viewerHTML))
	})

	addr := fmt.Sprintf(":%d", viewerPort)
	log.Printf("Viewer at http://localhost%s/", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Printf("viewer server error: %v", err)
	}
}

// serveRecordsAPI reads the JSONL file and returns records as a JSON array.
func serveRecordsAPI(w http.ResponseWriter, rec *recorder) {
	rec.mu.Lock()
	path := rec.file.Name()
	rec.mu.Unlock()

	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "failed to read records", http.StatusInternalServerError)
		return
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

	if records == nil {
		records = []json.RawMessage{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(records)
}
