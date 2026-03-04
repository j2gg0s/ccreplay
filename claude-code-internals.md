# Claude Code Internals: API Request Assembly

Based on reverse-engineering through proxy capture (`ccreplay.jsonl`) and local session files
(`~/.claude/projects/<project>/`).

## 1. Local Session Storage

### Location
```
~/.claude/projects/-Users-<user>-<path-to-project>/
├── <session-id>.jsonl          # conversation history
├── <session-id>/               # session artifacts (optional)
│   ├── subagents/              # sub-agent records
│   └── tool-results/           # tool execution results
└── memory/                     # persistent memory across sessions
```

Project path is encoded by replacing `/` with `-`.

### JSONL Record Types

Each line is a JSON object with a `type` field:

| type | description |
|------|-------------|
| `user` | User message or tool_result |
| `assistant` | Model response (thinking, tool_use, text) |
| `system` | System event |
| `progress` | Progress indicator |
| `file-history-snapshot` | File state snapshot for undo/rollback |

### Message Structure (local)

Each `user`/`assistant` record contains metadata:
```json
{
  "parentUuid": "...",
  "isSidechain": false,
  "userType": "external",
  "cwd": "/path/to/project",
  "sessionId": "uuid",
  "version": "2.1.66",
  "gitBranch": "main",
  "type": "user",
  "message": { "role": "user", "content": "..." },
  "uuid": "...",
  "timestamp": "...",
  "permissionMode": "..."
}
```

Key: assistant responses are stored as **separate lines per block** (one line for thinking,
one for tool_use, one for text), not merged into a single message.

## 2. API Request Assembly

### What Gets Sent (but NOT stored locally)

| Component | Description | Size (typical) |
|-----------|-------------|----------------|
| System prompt | Role definition, tool usage rules, git protocol, tone guidelines | ~18,858 chars, 3 blocks |
| Tools | Full JSON Schema for all tools (Agent, Bash, Glob, Grep, Read, Edit, Write, etc.) | ~39 tools |
| `<system-reminder>` | CLAUDE.md, skill list, git status, task reminders | Injected into user messages |
| Compact summary | Compressed conversation history from previous session | Injected into first user message on continue |

### Assembly Steps

```
Local JSONL → Merge → Inject → Cache Mark → Send
```

#### Step 1: Merge consecutive same-role messages

Local JSONL stores each block as a separate line:
```
line 1: { type: "assistant", message: { content: [{ type: "thinking", ... }] } }
line 2: { type: "assistant", message: { content: [{ type: "tool_use", ... }] } }
```

API request merges them into one message:
```json
{ "role": "assistant", "content": [
  { "type": "thinking", "thinking": "...", "signature": "..." },
  { "type": "tool_use", "name": "Bash", "id": "toolu_...", "input": {...} }
]}
```

Same for parallel tool_results — multiple local lines become one user message with
multiple `tool_result` blocks.

#### Step 2: Inject system prompt and tools

Every request includes:
```json
{
  "model": "claude-opus-4-6",
  "system": [
    { "type": "text", "text": "..." },                    // block 0
    { "type": "text", "text": "You are Claude Code..." }, // block 1 (cached)
    { "type": "text", "text": "..." }                     // block 2 (cached)
  ],
  "tools": [ /* 39 tool definitions */ ],
  "messages": [ /* assembled messages */ ]
}
```

#### Step 3: Inject `<system-reminder>` into user messages

Certain user messages get additional text blocks prepended/appended:

- **First user message**: CLAUDE.md content, git status
- **Periodic tool_result messages**: skill list, task reminders, file change notifications

Example — a simple user input `"fix the bug"` becomes:
```json
{ "role": "user", "content": [
  { "type": "text", "text": "<system-reminder>\n...CLAUDE.md content...\n</system-reminder>" },
  { "type": "text", "text": "fix the bug" }
]}
```

#### Step 4: Mark `cache_control`

`cache_control` is NOT stored locally — it's dynamically positioned on each request:

- **System prompt**: blocks 1 and 2 marked with `{ "type": "ephemeral", "ttl": "1h" }`
- **Messages**: the last 2 messages (typically the last assistant + last user) get cache marks
- Cache marks **shift forward** with each new request (this is why consecutive requests show
  "changed" messages — only the cache_control annotation moved)

```json
{ "type": "tool_use", "name": "Read", "id": "toolu_...",
  "cache_control": { "type": "ephemeral", "ttl": "1h" } }
```

Uses Anthropic's `prompt-caching-scope-2026-01-05` beta feature.

#### Step 5: Send

HTTP POST to `/v1/messages?beta=true` with headers:
```
Anthropic-Beta: claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14,
                context-management-2025-06-27,prompt-caching-scope-2026-01-05,
                effort-2025-11-24,adaptive-thinking-2026-01-28
Anthropic-Version: 2023-06-01
User-Agent: claude-cli/2.1.66 (external, cli)
```

## 3. Message Growth Pattern

Each API call in an agentic loop adds exactly **+2 messages**:
```
Request N = Request N-1 messages + assistant response + user tool_result/input
```

Example from a session:
```
req[186]: msgs=1    (first request after compact)
req[187]: msgs=201  (continue: loaded 200 messages from previous session)
req[188]: msgs=203  (+2)
req[189]: msgs=205  (+2)
...
req[264]: msgs=355  (+2 each time)
```

## 4. Session Boundaries and Compact

### New session detection (from proxy logs)
- `msgs=1`: brand new request or sub-agent call
- `msgs` drops significantly: compact happened or new session started

### Compact behavior
1. All conversation history compressed into a summary by a separate API call
2. Summary injected as a text block in the first user message of the new context
3. Local JSONL continues appending — the compact summary is NOT stored locally,
   it's regenerated or cached by the client

### Continue behavior
1. Find the most recently modified `.jsonl` in the project directory
2. Read all `user`/`assistant` messages
3. Re-assemble using the steps above (merge, inject, cache mark)
4. System prompt and tools come from the **current** Claude Code version (not the
   version that created the session)

## 5. Sub-agent Calls

Identified by:
- `msgs=1` with model `claude-haiku-4-5-*`
- Separate from the main conversation chain
- Used for: WebSearch, file analysis, code exploration, quota checks

Example patterns:
```
req[0]:   msgs=1, model=haiku    # "Files modified by user" check
req[151]: msgs=1, model=haiku    # WebSearch query
req[167]: msgs=1, model=haiku    # quota check
```

Sub-agents can also use opus for complex tasks (e.g., code review, summarization):
```
req[77]: msgs=1, model=opus     # HTML analysis
req[78]: msgs=1, model=opus     # file content analysis
```

## 6. Content Block Types in Messages

### User message blocks
| type | description |
|------|-------------|
| `text` | User input or injected system-reminder |
| `tool_result` | Result of a tool execution, linked by `tool_use_id` |

### Assistant message blocks
| type | description |
|------|-------------|
| `thinking` | Extended thinking (has `signature` field for verification) |
| `text` | Natural language response |
| `tool_use` | Tool invocation with `name`, `id`, `input` |

### Tool use → Tool result flow
```
assistant: { type: "tool_use", name: "Bash", id: "toolu_abc123", input: { command: "ls" } }
    ↓ (Claude Code executes the tool locally)
user:      { type: "tool_result", tool_use_id: "toolu_abc123", content: "file1.go\nfile2.go" }
```

## 7. Key Observations

1. **Local JSONL ≈ 80% of API request** — missing system prompt, tools, system-reminders,
   cache_control
2. **System prompt and tools update with CLI version** — old sessions get new prompts on continue
3. **Prompt caching is aggressive** — marks the tail of the conversation to avoid re-processing
   the entire history on each call
4. **No server-side state** — everything needed to reconstruct a session is local
5. **Thinking signatures** — stored locally, passed through to API for verification
