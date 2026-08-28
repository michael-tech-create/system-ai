// Command arctechture scans a Go project and asks Gemini for a structural
// architecture review. v1 is read-only: it never moves, renames, or
// edits any file. It only prints a report. That's intentional — see the
// project README for why the "apply" feature comes later, not first.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"arctechture/internal/llm"
	"arctechture/internal/scanner"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		if err != errUsage {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
		}
		os.Exit(1)
	}
}

var errUsage = fmt.Errorf("invalid usage")

func run(args []string) error {
	if len(args) < 2 || args[0] != "scan" {
		printUsage()
		return errUsage
	}

	projectPath := args[1]

	maxFiles := scanner.DefaultMaxFiles
	if v := strings.TrimSpace(os.Getenv("ARCTECHTURE_MAX_FILES")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return fmt.Errorf("ARCTECHTURE_MAX_FILES must be a positive integer")
		}
		maxFiles = n
	}

	timeout := 3 * time.Minute
	if v := strings.TrimSpace(os.Getenv("ARCTECHTURE_TIMEOUT")); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return fmt.Errorf("ARCTECHTURE_TIMEOUT must be a positive duration (e.g. 2m, 90s)")
		}
		timeout = d
	}

	// Validate and scan before requiring API keys so path mistakes fail fast.
	fmt.Printf("Scanning %s ...\n", projectPath)
	summary, err := scanner.ScanProjectWithOptions(projectPath, scanner.Options{MaxFiles: maxFiles})
	if err != nil {
		return fmt.Errorf("scanning project: %w", err)
	}

	if len(summary.Warnings) > 0 {
		fmt.Fprintf(os.Stderr, "Warnings (%d):\n", len(summary.Warnings))
		for _, w := range summary.Warnings {
			fmt.Fprintf(os.Stderr, "  - %s\n", w)
		}
		fmt.Fprintln(os.Stderr)
	}

	if summary.Truncated {
		fmt.Fprintf(os.Stderr, "Note: scan truncated at %d files (set ARCTECHTURE_MAX_FILES to raise the limit).\n\n", maxFiles)
	}

	if len(summary.Files) == 0 {
		fmt.Println("No Go files found. Nothing to review.")
		return nil
	}
	fmt.Printf("Found %d Go files. Sending structure to Gemini for review...\n\n", len(summary.Files))

	keysEnv := os.Getenv("GEMINI_API_KEYS")
	if keysEnv == "" {
		return fmt.Errorf("GEMINI_API_KEYS environment variable is not set (use comma-separated keys)")
	}
	apiKeys := parseAPIKeys(keysEnv)
	if len(apiKeys) == 0 {
		return fmt.Errorf("no valid API keys found in GEMINI_API_KEYS")
	}

	summaryJSON, err := json.Marshal(summary)
	if err != nil {
		return fmt.Errorf("encoding summary: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	client := llm.NewClient(apiKeys)
	review, err := client.Review(ctx, string(summaryJSON))
	if err != nil {
		return fmt.Errorf("getting review: %w", err)
	}

	printReport(review)
	return nil
}

func parseAPIKeys(keysEnv string) []string {
	rawKeys := strings.Split(keysEnv, ",")
	var apiKeys []string
	for _, k := range rawKeys {
		if trimmed := strings.TrimSpace(k); trimmed != "" {
			// Allow accidental quotes around individual keys or the whole value.
			trimmed = strings.Trim(trimmed, `"'`)
			if trimmed != "" {
				apiKeys = append(apiKeys, trimmed)
			}
		}
	}
	return apiKeys
}

// printReport renders the structured Review as readable terminal output.
// Kept separate from main() so the rendering logic can be swapped out
// later (e.g. for a TUI) without touching the scan/review flow.
func printReport(r *llm.Review) {
	fmt.Println("=== Architecture Review ===")
	fmt.Println()
	if strings.TrimSpace(r.Summary) != "" {
		fmt.Println(r.Summary)
		fmt.Println()
	}

	if len(r.Issues) > 0 {
		fmt.Println("--- Issues ---")
		for i, issue := range r.Issues {
			fmt.Printf("%d. [%s] %s\n", i+1, issue.Type, issue.Description)
			if len(issue.Files) > 0 {
				fmt.Printf("   files: %s\n", strings.Join(issue.Files, ", "))
			}
		}
		fmt.Println()
	}

	if len(r.Suggestions) > 0 {
		fmt.Println("--- Suggestions (not applied — this is a read-only report) ---")
		for i, s := range r.Suggestions {
			fmt.Printf("%d. %s: %s -> %s\n", i+1, s.Action, s.From, s.To)
			fmt.Printf("   reason: %s\n", s.Reason)
		}
		fmt.Println()
	}

	if len(r.Issues) == 0 && len(r.Suggestions) == 0 {
		fmt.Println("No structural issues or suggestions reported.")
	}
}

func printUsage() {
	fmt.Println("arctechture - AI-assisted architecture review for Go projects")
	fmt.Println()
	fmt.Println("Usage:")
	fmt.Println("  arctechture scan <path>    Scan a project and print an architecture review")
	fmt.Println()
	fmt.Println("Environment Variables:")
	fmt.Println("  GEMINI_API_KEYS            Comma-separated list of Gemini API keys (required)")
	fmt.Println("  GEMINI_MODEL               Model id (default: gemini-3.5-flash)")
	fmt.Println("  GEMINI_API_URL             Full generateContent URL override (optional)")
	fmt.Println("  ARCTECHTURE_MAX_FILES      Max Go files to include (default: 2000)")
	fmt.Println("  ARCTECHTURE_TIMEOUT        Overall review timeout (default: 3m)")
}
