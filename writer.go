package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// writeFinalDoc reads every batch part file (in order) and concatenates
// them into one Markdown file at outPath, with a small YAML front matter
// block carrying provenance (source, page count, dpi, model, batch size,
// timestamp) that a downstream RAG indexer can parse without this program.
func writeFinalDoc(outPath string, doc document, cfg config, pageCount int, partPaths []string) error {
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}

	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "source: %q\n", doc.sourcePath)
	fmt.Fprintf(&b, "source_type: %s\n", doc.sourceType())
	fmt.Fprintf(&b, "page_count: %d\n", pageCount)
	if doc.isPDF {
		fmt.Fprintf(&b, "dpi: %g\n", cfg.dpi)
	}
	fmt.Fprintf(&b, "batch_size: %d\n", cfg.batchSize)
	fmt.Fprintf(&b, "model: %q\n", cfg.model)
	fmt.Fprintf(&b, "generated_at: %q\n", time.Now().UTC().Format(time.RFC3339))
	b.WriteString("---\n\n")

	for i, p := range partPaths {
		part, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("read part %d: %w", i+1, err)
		}
		b.WriteString(strings.TrimSpace(string(part)))
		b.WriteString("\n\n")
	}

	tmp := outPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.TrimRight(b.String(), "\n")+"\n"), 0o644); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := os.Rename(tmp, outPath); err != nil {
		return fmt.Errorf("rename into place: %w", err)
	}
	return nil
}

// manifestRecord is one line of manifest.jsonl: a flat, indexer-friendly
// description of what happened to each source document.
type manifestRecord struct {
	Status     string `json:"status"` // "ok" | "failed" | "skipped"
	Source     string `json:"source"`
	SourceType string `json:"source_type"`
	PageCount  int    `json:"page_count,omitempty"`
	OutputPath string `json:"output_path,omitempty"`
	Error      string `json:"error,omitempty"`
	Timestamp  string `json:"timestamp"`
}

type manifestWriter struct {
	path string
	f    *os.File
	enc  *json.Encoder
}

func newManifestWriter(path string) *manifestWriter {
	f, err := os.Create(path)
	if err != nil {
		log.Fatalf("go-ai-ocr: create manifest %s: %v", path, err)
	}
	return &manifestWriter{path: path, f: f, enc: json.NewEncoder(f)}
}

func (m *manifestWriter) write(r manifestRecord) {
	r.Timestamp = time.Now().UTC().Format(time.RFC3339)
	if err := m.enc.Encode(r); err != nil {
		log.Printf("go-ai-ocr: manifest write failed: %v", err)
	}
}

func (m *manifestWriter) WriteSuccess(doc document, outPath string, pageCount int) {
	m.write(manifestRecord{
		Status: "ok", Source: doc.sourcePath, SourceType: doc.sourceType(),
		PageCount: pageCount, OutputPath: outPath,
	})
}

func (m *manifestWriter) WriteFailure(doc document, err error) {
	m.write(manifestRecord{
		Status: "failed", Source: doc.sourcePath, SourceType: doc.sourceType(),
		Error: err.Error(),
	})
}

func (m *manifestWriter) WriteSkipped(doc document, outPath string) {
	m.write(manifestRecord{
		Status: "skipped", Source: doc.sourcePath, SourceType: doc.sourceType(),
		OutputPath: outPath,
	})
}

func (m *manifestWriter) Close() {
	if err := m.f.Close(); err != nil {
		log.Printf("go-ai-ocr: closing manifest: %v", err)
	}
}
