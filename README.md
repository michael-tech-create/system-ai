# Arctechture

**Author:** Michael Unogwu  
**Version:** 1.0.0 (Read-Only)

`arctechture` is a lightweight command-line utility built in Go that performs AI-assisted architectural reviews of Go codebases. Instead of dumping entire source files into an LLM prompt, it uses Abstract Syntax Tree (AST) parsing to generate a compact, token-efficient JSON summary of your project's structure, then routes it through Google's Gemini API with multi-key rate-limit fallback.

## Features

* **AST-based scanning** — Recursively parses Go files with `go/parser` and `go/ast` to extract packages, imports, types, and top-level functions/methods (including receiver form `T.Method` / `*T.Method`) without sending raw source.
* **Resilient multi-key rotation** — Thread-safe key pool tracks HTTP 429/5xx, quota errors, and auth failures; cycles keys with exponential cooldown and respects an overall timeout/cancel.
* **Strict JSON reviews** — Asks Gemini for structured JSON (`summary`, `issues`, `suggestions`) via `responseMimeType: application/json`.
* **Read-only safety** — v1 only prints a report. It never moves, renames, or edits files.
* **Scan limits & warnings** — Caps file count (default 2000), skips noisy dirs, and surfaces parse/walk warnings instead of failing the whole run.

## Project structure

| Component | Path | Description |
| :--- | :--- | :--- |
| **CLI** | `main.go` | Argument parsing, env config, report rendering |
| **AST scanner** | `internal/scanner` | Directory walk + structural JSON payload |
| **LLM client** | `internal/llm` | Gemini HTTP transport, prompts, key pool |

## Requirements

* Go 1.22+ (developed on Go 1.26)
* One or more Gemini API keys

## Setup

```bash
cd arctechture

# Required: comma-separated Gemini API keys (do not commit keys)
export GEMINI_API_KEYS="your_key_1,your_key_2"

# Optional
export GEMINI_MODEL="gemini-3.5-flash"   # default (avoid gemini-3.6-flash — can hang)
export ARCTECHTURE_MAX_FILES=2000        # default
export ARCTECHTURE_TIMEOUT=3m            # default
```

Keep secrets in the environment or a gitignored `.env` loader of your choice. Rotate any key that may have been exposed.

## Usage

```bash
# From source
go run . scan /path/to/project

# Or install a binary
go build -o arctechture .
./arctechture scan .
```

Example flow:

```text
$ go run . scan .
Scanning . ...
Found 3 Go files. Sending structure to Gemini for review...

=== Architecture Review ===

...
```

## Tests

```bash
go test ./...
```

## Design notes

* Full file contents are never sent to the model — only the structural summary.
* API keys are sent via the `x-goog-api-key` header (not the URL query string).
* An "apply suggestions" mode is intentionally deferred until review quality is proven.
