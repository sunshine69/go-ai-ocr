package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// writeFinalDoc reads every batch part file (in order) and concatenates
// them into one Markdown file at outPath. Any ```markdown fence the model
// emits around a part is stripped (see unwrapMarkdown), and a small YAML
// front-matter block carrying provenance (source, page count, dpi, model,
// batch size, timestamp) is prepended, which a downstream RAG indexer can
// parse without this program.
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
		// Strip any ```markdown ``` fence the model emits. The model is
		// told (see the default prompt) to emit Markdown only, but many
		// models wrap their reply in a ```markdown fenced block out of
		// rendering habit, not because it's required — so this must
		// survive code-side, not on the model's good behaviour. The final
		// .md is fed to a downstream RAG indexer, which wants raw
		// Markdown text with no wrapper. Unwrap each part independently
		// so only one batch carrying a fence doesn't sink the document.
		b.WriteString(string(unwrapMarkdown(part)))
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

// unwrapMarkdown strips a single leading ```markdown fence block from the
// model's output, returning the raw Markdown it wrapped. The final .md file
// exists to be read by a downstream RAG indexer that expects pure Markdown
// text — a ```markdown wrapper is only a rendering aid and must not survive
// into the stored document.
//
// The default prompt already asks the model to emit Markdown only, but many
// models default to wrapping their reply in a fenced block regardless. This
// is therefore applied defensively, independent of the model following the
// instruction: if a fence is present we peel it; if not we return the input
// untouched.
func unwrapMarkdown(b []byte) []byte {
	trimmed := bytes.TrimLeft(b, " \t\r\n")
	// Confirm it opens with a ``` fence line (e.g. "```markdown") on its
	// own line, allowing optional leading/whitespace and a language tag.
	idx := bytes.IndexByte(trimmed, '\n')
	if idx == -1 {
		return bytes.TrimSpace(b)
	}
	openLine := strings.ToLower(strings.TrimSpace(string(trimmed[:idx])))
	if !strings.HasPrefix(openLine, "```") || len(openLine) < 4 {
		return bytes.TrimSpace(b)
	}
	lang := strings.ToLower(strings.TrimPrefix(openLine[3:], "markdown"))
	// Accept the fence as a markdown fence when the tag is empty or is a
	// well-known "markdown" qualifier (e.g. "markdown-fenced").
	if !(lang == "" || strings.HasPrefix(lang, "-") || strings.HasPrefix(lang, "_") || strings.HasPrefix(lang, " fenced")) {
		return bytes.TrimSpace(b)
	}
	// Find the closing fence; anything before it is the wrapped content.
	content := trimmed[idx+1:]
	closing := bytes.Index(content, []byte("```"))
	if closing == -1 {
		// No closing fence: leave the input alone rather than guess.
		return bytes.TrimSpace(b)
	}
	return bytes.TrimSpace(content[:closing])
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
