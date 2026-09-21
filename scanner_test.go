package main

import (
	"path/filepath"
	"testing"
)

// TestDiscoverSingleFile verifies that a single-file input (not a directory)
// produces a document whose relDir is ".", so the output lands in the output
// root itself (not its parent). This reproduces the -o resources/parsed1
// bug where relDir came out as ".." and the .md was written to resources/
// instead of resources/parsed1/.
func TestDiscoverSingleFile(t *testing.T) {
	dir := t.TempDir()
	// A PDF file at a path (as opposed to under a directory root).
	pdfPath := filepath.Join(dir, "Invoice INV-3556.pdf")
	pdf, err := testPdf(pdfPath)
	if err != nil {
		t.Fatal(err)
	}
	defer pdf.Close()

	cfg := config{inputDir: pdfPath}
	docs, err := discoverDocuments(cfg)
	if err != nil {
		t.Fatalf("discoverDocuments: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("got %d documents, want 1", len(docs))
	}
	d := docs[0]
	if d.sourcePath != pdfPath {
		t.Errorf("sourcePath = %q, want %q", d.sourcePath, pdfPath)
	}
	if d.baseName != "Invoice INV-3556" {
		t.Errorf("baseName = %q, want %q", d.baseName, "Invoice INV-3556")
	}
	if !d.isPDF {
		t.Errorf("isPDF = %v, want true", d.isPDF)
	}
	// The key assertion: relDir must be "." (the file IS the input root),
	// not "..".
	if d.relDir != "." {
		t.Errorf("relDir = %q, want %q", d.relDir, ".")
	}
}

// TestDiscoverSingleFileResolveOutputPath confirms that with a single-file
// input the resolved output path sits directly inside the output root,
// not in its parent directory.
func TestDiscoverSingleFileResolveOutputPath(t *testing.T) {
	dir := t.TempDir()
	pdfPath := filepath.Join(dir, "Invoice INV-3556.pdf")
	pdf, err := testPdf(pdfPath)
	if err != nil {
		t.Fatal(err)
	}
	defer pdf.Close()

	cfg := config{inputDir: pdfPath}
	docs, err := discoverDocuments(cfg)
	if err != nil {
		t.Fatalf("discoverDocuments: %v", err)
	}
	d := docs[0]

	// Mirror what main does: resolve the output path against the output root
	// and confirm it lands in outputRoot/<base>.md, not in outputRoot/parent.
	outputRoot := filepath.Join(dir, "resources", "parsed1")
	state, err := newStateDB(filepath.Join(dir, "state.sqlite3"))
	if err != nil {
		t.Fatalf("newStateDB: %v", err)
	}
	defer state.Close()

	outPath, suffix, err := state.resolveOutputPath(d.sourcePath, outputRoot, "deadbeef", d.relDir)
	if err != nil {
		t.Fatalf("resolveOutputPath: %v", err)
	}
	want := filepath.Join(outputRoot, "Invoice INV-3556.md")
	if outPath != want {
		t.Errorf("resolveOutputPath = %q, want %q", outPath, want)
	}
	if suffix != "" {
		t.Errorf("suffix = %q, want empty (no collision)", suffix)
	}
}
