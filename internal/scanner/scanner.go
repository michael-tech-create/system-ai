// Package scanner walks a Go project's directory tree and builds a
// lightweight structural summary of it: file paths, package names,
// imports, and top-level declarations (funcs/types/structs).
//
// The key design decision here: we NEVER send full file contents to the
// LLM. We only send this summary. That keeps token usage low even on a
// large repo, and it's enough context for the model to spot structural
// problems (misplaced files, circular deps, inconsistent packages)
// without needing to read every line of code.
package scanner

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// DefaultMaxFiles caps how many Go files are included in a summary so
// large monorepos cannot blow the LLM context window unnoticed.
const DefaultMaxFiles = 2000

// FileSummary holds everything the LLM needs to reason about one Go file,
// without including the actual source code.
type FileSummary struct {
	Path      string   `json:"path"`       // relative path from project root
	Package   string   `json:"package"`    // declared package name
	Imports   []string `json:"imports"`    // import paths used by this file
	Functions []string `json:"functions"`  // top-level funcs; methods as Type.Method
	Types     []string `json:"types"`      // top-level type/struct names
	Lines     int      `json:"line_count"` // rough size signal
}

// ProjectSummary is the full scan result for a project.
type ProjectSummary struct {
	RootPath string        `json:"root_path"`
	Files    []FileSummary `json:"files"`
	Warnings []string      `json:"warnings,omitempty"`
	Truncated bool         `json:"truncated,omitempty"`
}

// dirsToSkip are directories we never want to walk into. Vendored code,
// build output, and VCS internals are noise for architecture analysis.
var dirsToSkip = map[string]bool{
	".git":         true,
	"vendor":       true,
	"node_modules": true,
	"dist":         true,
	"build":        true,
	"bin":          true,
	"testdata":     true,
	"third_party":  true,
	".idea":        true,
	".vscode":      true,
}

// Options controls scan behavior.
type Options struct {
	// MaxFiles limits how many .go files are kept (0 = DefaultMaxFiles).
	MaxFiles int
}

// ScanProject walks rootPath and returns a structural summary of every
// .go file it finds. Errors on individual files are collected as warnings
// and do not stop the scan — a single malformed file shouldn't block
// analysis of the rest of the project.
func ScanProject(rootPath string) (*ProjectSummary, error) {
	return ScanProjectWithOptions(rootPath, Options{})
}

// ScanProjectWithOptions is like ScanProject but accepts scan limits.
func ScanProjectWithOptions(rootPath string, opts Options) (*ProjectSummary, error) {
	info, err := os.Stat(rootPath)
	if err != nil {
		return nil, fmt.Errorf("project path: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("project path is not a directory: %s", rootPath)
	}

	maxFiles := opts.MaxFiles
	if maxFiles <= 0 {
		maxFiles = DefaultMaxFiles
	}

	summary := &ProjectSummary{RootPath: rootPath}

	err = filepath.WalkDir(rootPath, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			summary.Warnings = append(summary.Warnings,
				fmt.Sprintf("skip %s: %v", path, walkErr))
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if d.IsDir() {
			if dirsToSkip[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}

		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		// Skip generated/test files for v1 — they'd add noise to the
		// architecture picture without adding much signal.
		base := filepath.Base(path)
		if strings.HasSuffix(base, "_test.go") {
			return nil
		}
		if strings.HasPrefix(base, ".") {
			return nil
		}

		if len(summary.Files) >= maxFiles {
			summary.Truncated = true
			return fs.SkipAll
		}

		fileSum, parseErr := parseGoFile(path)
		if parseErr != nil {
			rel := path
			if r, relErr := filepath.Rel(rootPath, path); relErr == nil {
				rel = r
			}
			summary.Warnings = append(summary.Warnings,
				fmt.Sprintf("parse %s: %v", rel, parseErr))
			return nil
		}

		relPath, relErr := filepath.Rel(rootPath, path)
		if relErr == nil {
			fileSum.Path = relPath
		} else {
			fileSum.Path = path
		}

		summary.Files = append(summary.Files, *fileSum)
		return nil
	})
	if err != nil {
		return summary, fmt.Errorf("walk project: %w", err)
	}

	return summary, nil
}

// parseGoFile extracts a structural summary from a single Go source file
// using the standard library's own parser — no external dependencies.
func parseGoFile(path string) (*FileSummary, error) {
	fset := token.NewFileSet()

	// parser.ParseComments isn't needed here since we only want structure,
	// not documentation — keeps parsing a bit faster.
	node, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil, err
	}

	fileSum := &FileSummary{
		Package: node.Name.Name,
	}

	for _, imp := range node.Imports {
		// imp.Path.Value includes the surrounding quotes, e.g. `"fmt"`.
		fileSum.Imports = append(fileSum.Imports, strings.Trim(imp.Path.Value, `"`))
	}

	// Walk top-level declarations to pull out function and type names.
	// We only care about top-level (package-scope) declarations for the
	// architecture picture — nested/local types add noise.
	for _, decl := range node.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			fileSum.Functions = append(fileSum.Functions, funcName(d))
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				if ts, ok := spec.(*ast.TypeSpec); ok {
					fileSum.Types = append(fileSum.Types, ts.Name.Name)
				}
			}
		}
	}

	// Line count from the fset gives us a cheap size signal without
	// re-reading the file.
	fileSum.Lines = fset.Position(node.End()).Line

	return fileSum, nil
}

// funcName formats a function or method for the summary.
// Methods include the receiver type: "T.Method" or "*T.Method".
func funcName(d *ast.FuncDecl) string {
	if d.Recv == nil || len(d.Recv.List) == 0 {
		return d.Name.Name
	}

	recv := recvTypeName(d.Recv.List[0].Type)
	if recv == "" {
		return d.Name.Name
	}
	return recv + "." + d.Name.Name
}

func recvTypeName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		inner := recvTypeName(t.X)
		if inner == "" {
			return ""
		}
		return "*" + inner
	case *ast.IndexExpr:
		return recvTypeName(t.X)
	case *ast.IndexListExpr:
		return recvTypeName(t.X)
	default:
		return ""
	}
}
