# Arctechture

**Author:** Michael Unogwu  
**Version:** 1.0.0 (Read-Only)


arctechture` is a lightweight, high-performance command-line utility built in Go that performs AI-assisted architectural reviews of Go codebases. Instead of dumping entire source files into an LLM prompt, it uses Abstract Syntax Tree (AST) parsing to generate a compact, token-efficient JSON summary of your project's structure, routing it through Google's Gemini API with a built-in multi-key rate-limit fallback system.

## Features

* **AST-Based Code Scanning:** Recursively parses Go files using standard library tools (`go/parser` and `go/ast`) to extract package declarations, file paths, imports, and top-level functions/types without exposing raw source implementation details.
* **Resilient Multi-Key Rotation:** Thread-safe key management engine (`keyManager`) tracks HTTP `429 Too Many Requests` responses and quota errors, automatically cycling through alternative API keys during heavy batch executions.
* **Strict JSON Enforcement:** Integrates with Gemini (`gemini-3.6-flash`) using strict MIME type generation configs to guarantee valid, machine-parsable architectural analysis output.
* **Read-Only Safety:** Operates strictly read-only in v1 to evaluate package boundaries, circular imports, and misplaced files safely before automated refactoring features are introduced.

## Project Structure

| Component | Path | Description |
| :--- | :--- | :--- |
| **CLI Controller** | `main.go` | Handles terminal argument parsing, execution routing, and report rendering. |
| **AST Scanner** | `internal/scanner` | Recursively walks project directories to build the structural JSON payload. |
| **LLM Transport** | `internal/llm` | Manages HTTP client requests, system prompt payload marshaling, and thread-safe API key pools. |

## Installation & Setup

1. Clone the repository and navigate to the project root:
   ```bash
   cd arctechture

   go run main.go scan .