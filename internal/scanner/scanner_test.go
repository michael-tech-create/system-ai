package scanner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScanProjectSample(t *testing.T) {
	root := filepath.Join("testdata", "sample")
	sum, err := ScanProject(root)
	if err != nil {
		t.Fatalf("ScanProject: %v", err)
	}
	if len(sum.Files) != 2 {
		t.Fatalf("expected 2 go files, got %d: %+v", len(sum.Files), filePaths(sum))
	}

	byPath := map[string]FileSummary{}
	for _, f := range sum.Files {
		byPath[filepath.ToSlash(f.Path)] = f
	}

	mainFile, ok := byPath["main.go"]
	if !ok {
		t.Fatalf("main.go missing: %+v", byPath)
	}
	if mainFile.Package != "main" {
		t.Errorf("main package = %q", mainFile.Package)
	}
	if !contains(mainFile.Functions, "main") {
		t.Errorf("main functions = %v, want main", mainFile.Functions)
	}
	if !contains(mainFile.Imports, "fmt") || !contains(mainFile.Imports, "example.com/sample/pkg") {
		t.Errorf("main imports = %v", mainFile.Imports)
	}

	greeter, ok := byPath["pkg/greeter.go"]
	if !ok {
		t.Fatalf("pkg/greeter.go missing: %+v", byPath)
	}
	if greeter.Package != "pkg" {
		t.Errorf("greeter package = %q", greeter.Package)
	}
	if !contains(greeter.Types, "Greeter") || !contains(greeter.Types, "helper") {
		t.Errorf("types = %v", greeter.Types)
	}
	if !contains(greeter.Functions, "Greeter.Greet") {
		t.Errorf("expected Greeter.Greet in %v", greeter.Functions)
	}
	if !contains(greeter.Functions, "*helper.secret") {
		t.Errorf("expected *helper.secret in %v", greeter.Functions)
	}
	if !contains(greeter.Functions, "NewGreeter") {
		t.Errorf("expected NewGreeter in %v", greeter.Functions)
	}
	if greeter.Lines <= 0 {
		t.Errorf("expected positive line count, got %d", greeter.Lines)
	}
}

func TestScanProjectRejectsFilePath(t *testing.T) {
	f := filepath.Join("testdata", "sample", "main.go")
	_, err := ScanProject(f)
	if err == nil {
		t.Fatal("expected error for file path")
	}
}

func TestScanProjectRejectsMissing(t *testing.T) {
	_, err := ScanProject(filepath.Join("testdata", "does-not-exist"))
	if err == nil {
		t.Fatal("expected error for missing path")
	}
}

func TestScanProjectSkipsTestdataNested(t *testing.T) {
	// Create a temp project that contains a nested testdata dir with a .go file.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "root.go"), []byte("package root\nfunc A() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(dir, "testdata", "ignored")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "x.go"), []byte("package ignored\nfunc B() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sum, err := ScanProject(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Files) != 1 {
		t.Fatalf("expected only root.go, got %+v", filePaths(sum))
	}
	if sum.Files[0].Package != "root" {
		t.Errorf("package = %q", sum.Files[0].Package)
	}
}

func TestScanProjectMaxFilesTruncation(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 5; i++ {
		name := filepath.Join(dir, "f"+string(rune('a'+i))+".go")
		body := "package p\nfunc F" + string(rune('A'+i)) + "() {}\n"
		if err := os.WriteFile(name, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sum, err := ScanProjectWithOptions(dir, Options{MaxFiles: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !sum.Truncated {
		t.Fatal("expected Truncated=true")
	}
	if len(sum.Files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(sum.Files))
	}
}

func TestScanProjectParseWarning(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ok.go"), []byte("package p\nfunc Ok() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bad.go"), []byte("package p\nfunc (\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sum, err := ScanProject(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Files) != 1 {
		t.Fatalf("expected 1 good file, got %d", len(sum.Files))
	}
	if len(sum.Warnings) == 0 {
		t.Fatal("expected parse warning")
	}
	if !strings.Contains(sum.Warnings[0], "bad.go") {
		t.Errorf("warning = %q", sum.Warnings[0])
	}
}

func TestFuncNameFormatting(t *testing.T) {
	dir := t.TempDir()
	src := `package p
type T struct{}
func (t T) M() {}
func (t *T) P() {}
func F() {}
`
	path := filepath.Join(dir, "m.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	fs, err := parseGoFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"T.M", "*T.P", "F"}
	for _, w := range want {
		if !contains(fs.Functions, w) {
			t.Errorf("functions %v missing %s", fs.Functions, w)
		}
	}
}

func filePaths(sum *ProjectSummary) []string {
	var out []string
	for _, f := range sum.Files {
		out = append(out, f.Path)
	}
	return out
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
