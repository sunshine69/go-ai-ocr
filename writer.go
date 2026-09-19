package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// writeOutput writes the extracted text for one job to outPath as a Markdown
// file with a small YAML front matter block carrying provenance metadata
// (source file, page number, DPI, model, timestamp) that a downstream RAG
// indexer can parse without needing this program.
func writeOutput(outPath string, j job, cfg config, text string) error {
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}

	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "source: %q\n", j.sourcePath)
	if j.isPDF {
		b.WriteString("source_type: pdf\n")
		fmt.Fprintf(&b, "page: %d\n", j.pageNum)
		fmt.Fprintf(&b, "page_count: %d\n", j.pageCount)
		fmt.Fprintf(&b, "dpi: %g\n", cfg.dpi)
	} else {
		b.WriteString("source_type: image\n")
	}
	fmt.Fprintf(&b, "model: %q\n", cfg.model)
	fmt.Fprintf(&b, "generated_at: %q\n", time.Now().UTC().Format(time.RFC3339))
	b.WriteString("---\n\n")
	b.WriteString(strings.TrimSpace(text))
	b.WriteString("\n")

	tmp := outPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := os.Rename(tmp, outPath); err != nil {
		return fmt.Errorf("rename into place: %w", err)
	}
	return nil
}

// manifestRecord is one line of manifest.jsonl: a flat, indexer-friendly
// description of what happened to each source page/image.
type manifestRecord struct {
	Status     string `json:"status"` // "ok" | "failed" | "skipped"
	Source     string `json:"source"`
	SourceType string `json:"source_type"`
	Page       int    `json:"page,omitempty"`
	PageCount  int    `json:"page_count,omitempty"`
	OutputPath string `json:"output_path,omitempty"`
	Error      string `json:"error,omitempty"`
	Timestamp  string `json:"timestamp"`
}

type manifestWriter struct {
	path string
	mu   sync.Mutex
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
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.enc.Encode(r); err != nil {
		log.Printf("go-ai-ocr: manifest write failed: %v", err)
	}
}

func (m *manifestWriter) WriteSuccess(j job, outPath string) {
	m.write(manifestRecord{
		Status:     "ok",
		Source:     j.sourcePath,
		SourceType: sourceType(j),
		Page:       j.pageNum,
		PageCount:  j.pageCount,
		OutputPath: outPath,
	})
}

func (m *manifestWriter) WriteFailure(j job, err error) {
	m.write(manifestRecord{
		Status:     "failed",
		Source:     j.sourcePath,
		SourceType: sourceType(j),
		Page:       j.pageNum,
		PageCount:  j.pageCount,
		Error:      err.Error(),
	})
}

func (m *manifestWriter) WriteSkipped(j job, outPath string) {
	m.write(manifestRecord{
		Status:     "skipped",
		Source:     j.sourcePath,
		SourceType: sourceType(j),
		Page:       j.pageNum,
		PageCount:  j.pageCount,
		OutputPath: outPath,
	})
}

func (m *manifestWriter) Close() {
	if err := m.f.Close(); err != nil {
		log.Printf("go-ai-ocr: closing manifest: %v", err)
	}
}

func sourceType(j job) string {
	if j.isPDF {
		return "pdf"
	}
	return "image"
}
