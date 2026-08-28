// Command organizer scans a Go project and asks Gemini for a structural
// architecture review. v1 is read-only: it never moves, renames, or
// edits any file. It only prints a report. That's intentional — see the
// project README for why the "apply" feature comes later, not first.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"arctechture/internal/llm"
	"arctechture/internal/scanner"
)

func main() {
	if len(os.Args) < 3 || os.Args[1] != "scan" {
		printUsage()
		os.Exit(1)
	}

	projectPath := os.Args[2]

	keysEnv := os.Getenv("GEMINI_API_KEYS")
	if keysEnv == "" {
		fmt.Fprintln(os.Stderr, "error: GEMINI_API_KEYS environment variable is not set (use comma-separated keys)")
		os.Exit(1)
	}

	// Split and trim spaces from the comma-separated keys
	rawKeys := strings.Split(keysEnv, ",")
	var apiKeys []string
	for _, k := range rawKeys {
		if trimmed := strings.TrimSpace(k); trimmed != "" {
			apiKeys = append(apiKeys, trimmed)
		}
	}

	if len(apiKeys) == 0 {
		fmt.Fprintln(os.Stderr, "error: no valid API keys found in GEMINI_API_KEYS")
		os.Exit(1)
	}

	fmt.Printf("Scanning %s ...\n", projectPath)
	summary, err := scanner.ScanProject(projectPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error scanning project: %v\n", err)
		os.Exit(1)
	}

	if len(summary.Files) == 0 {
		fmt.Println("No Go files found. Nothing to review.")
		return
	}
	fmt.Printf("Found %d Go files. Sending structure to Gemini for review...\n\n", len(summary.Files))

	summaryJSON, err := json.Marshal(summary)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error encoding summary: %v\n", err)
		os.Exit(1)
	}

	client := llm.NewClient(apiKeys)
	review, err := client.Review(string(summaryJSON))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error getting review: %v\n", err)
		os.Exit(1)
	}

	printReport(review)
}

// printReport renders the structured Review as readable terminal output.
// Kept separate from main() so the rendering logic can be swapped out
// later (e.g. for a TUI) without touching the scan/review flow.
func printReport(r *llm.Review) {
	fmt.Println("=== Architecture Review ===")
	fmt.Println()
	fmt.Println(r.Summary)
	fmt.Println()

	if len(r.Issues) > 0 {
		fmt.Println("--- Issues ---")
		for i, issue := range r.Issues {
			fmt.Printf("%d. [%s] %s\n", i+1, issue.Type, issue.Description)
			if len(issue.Files) > 0 {
				fmt.Printf("   files: %v\n", issue.Files)
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
	}
}

func printUsage() {
	fmt.Println("organizer - AI-assisted architecture review for Go projects")
	fmt.Println()
	fmt.Println("Usage:")
	fmt.Println("  organizer scan <path>    Scan a project and print an architecture review")
	fmt.Println()
	fmt.Println("Environment Variables:")
	fmt.Println("  GEMINI_API_KEYS          Comma-separated list of Gemini API keys")
}
