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
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
)

// FileSummary holds everything the LLM needs to reason about one Go file,
// without including the actual source code.
type FileSummary struct {
	Path      string   `json:"path"`       // relative path from project root
	Package   string   `json:"package"`    // declared package name
	Imports   []string `json:"imports"`    // import paths used by this file
	Functions []string `json:"functions"`  // top-level function names
	Types     []string `json:"types"`      // top-level type/struct names
	Lines     int      `json:"line_count"` // rough size signal
}

// ProjectSummary is the full scan result for a project.
type ProjectSummary struct {
	RootPath string        `json:"root_path"`
	Files    []FileSummary `json:"files"`
}

// dirsToSkip are directories we never want to walk into. Vendored code,
// build output, and VCS internals are noise for architecture analysis.
var dirsToSkip = map[string]bool{
	".git":         true,
	"vendor":       true,
	"node_modules": true,
	"dist":         true,
	"build":        true,
	".idea":        true,
	".vscode":      true,
}

// ScanProject walks rootPath and returns a structural summary of every
// .go file it finds. Errors on individual files are collected but don't
// stop the scan — a single malformed file shouldn't block analysis of
// the rest of the project.
func ScanProject(rootPath string) (*ProjectSummary, error) {
	summary := &ProjectSummary{RootPath: rootPath}

	err := filepath.WalkDir(rootPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Skip files we can't even stat, but keep scanning the rest.
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
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}

		fs, parseErr := parseGoFile(path)
		if parseErr != nil {
			// A file that fails to parse (syntax error, etc.) is still
			// worth reporting on later — for now we just skip it rather
			// than aborting the whole scan.
			return nil
		}

		relPath, relErr := filepath.Rel(rootPath, path)
		if relErr == nil {
			fs.Path = relPath
		}

		summary.Files = append(summary.Files, *fs)
		return nil
	})

	return summary, err
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

	fs := &FileSummary{
		Package: node.Name.Name,
	}

	for _, imp := range node.Imports {
		// imp.Path.Value includes the surrounding quotes, e.g. `"fmt"`.
		fs.Imports = append(fs.Imports, strings.Trim(imp.Path.Value, `"`))
	}

	// Walk top-level declarations to pull out function and type names.
	// We only care about top-level (package-scope) declarations for the
	// architecture picture — nested/local types add noise.
	for _, decl := range node.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			fs.Functions = append(fs.Functions, d.Name.Name)
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				if ts, ok := spec.(*ast.TypeSpec); ok {
					fs.Types = append(fs.Types, ts.Name.Name)
				}
			}
		}
	}

	// Line count from the fset gives us a cheap size signal without
	// re-reading the file.
	fs.Lines = fset.Position(node.End()).Line

	return fs, nil
}