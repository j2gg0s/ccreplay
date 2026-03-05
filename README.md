# ccreplay

Record, inspect and replay Claude Code's API requests.

## Tips

- **Recommended: use with Claude Code** — run this project inside [Claude Code](https://docs.anthropic.com/en/docs/claude-code) for the best experience.
- **Ask Claude about unfamiliar messages** — if you see a message you don't understand in the viewer (e.g. `ToolSearch`, `system-reminder`), just ask Claude Code to explain it.
- **Reference specific rounds** — use `C0R1` to refer to Conversation #0, Round 1 when discussing with Claude Code. For example: "What does the tool_use in C0R1 do?"

## Usage

### 1. Start the proxy

```bash
go build . && ./ccreplay proxy
```

Flags:

| Flag | Default | Description |
|------|---------|-------------|
| `-listen` | `:9999` | Listen address |
| `-target` | `*.anthropic.com` | Target domain |
| `-output` | `.` | Output directory for `ccreplay.jsonl` |
| `-truncate` | `false` | Truncate log file on start (default: append) |

The proxy also starts a live viewer at `http://localhost:10000` (listen port + 1).

### 2. Run Claude Code through the proxy

```bash
ANTHROPIC_BASE_URL=http://127.0.0.1:9999 claude
```

All API requests are transparently forwarded to `api.anthropic.com` and recorded to `ccreplay.jsonl`.

> **Note:** Claude Code sends the full conversation history in every API request. The JSONL file grows much larger than you might expect. A short session can easily produce a multi-GB recording.

### 3. Inspect recorded traffic

**Live viewer** — visit `http://localhost:10000` while the proxy is running.

**Static HTML** — generate a self-contained HTML file from a JSONL recording:

```bash
./ccreplay show ccreplay.jsonl              # generates ccreplay.html and opens browser
./ccreplay show -o output.html ccreplay.jsonl
```

### 4. Replay a recorded request (CLI)

Re-send a recorded API request to compare responses:

```bash
./ccreplay replay -api-key sk-ant-xxx ccreplay.jsonl          # replay last record
./ccreplay replay -api-key sk-ant-xxx -record 7 ccreplay.jsonl  # replay specific record
./ccreplay replay -api-key sk-ant-xxx -no-stream ccreplay.jsonl  # force non-streaming
```

Outputs a side-by-side comparison of original vs replay (model, tokens, latency, content).

> **TODO:** Replay from the viewer frontend is not yet implemented.

## Viewer features

- **Conversation grouping** — API calls grouped into conversations and rounds (user interactions)
- **Round folding** — each round shows user message as summary with numbered `#N` prefix, API call details collapsed
- **Tool cards** — tool_use and tool_result paired together; results matched across all turns (handles context management)
- **Raw JSON** — per-API-call raw data with delta-only messages (starting from user), jq-style syntax highlighting, headers/metadata collapsed
- **SSE parsing** — streaming responses parsed into structured content blocks instead of raw event text
- **Subcalls** — background API calls (e.g. Haiku sub-agents) grouped separately
- **System prompt / Tools** — viewable per conversation
- **Drop zone** — drag & drop JSONL files when opened standalone

## Project structure

```
main.go      CLI entry point (proxy / show / replay)
proxy.go     HTTP reverse proxy with recording + live viewer
show.go      JSONL → self-contained HTML viewer
replay.go    Replay recorded requests for comparison
viewer.html  Embedded HTML/CSS/JS viewer template
```
