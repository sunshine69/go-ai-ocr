// Command go-ai-ocr scans a directory for PDFs and images, renders PDF pages
// to temporary page images via MuPDF (go-fitz), submits them to a
// vision-capable model served OpenAI-style (e.g. llama.cpp running
// Qwen2-VL) in small fixed-size batches, and writes one concatenated
// Markdown file per source document to an output directory ready for a
// downstream RAG indexer.
//
// This is intentionally single-threaded end to end: one document at a
// time, one batch at a time, one AI request at a time. There is no worker
// pool and no concurrency to tune — the batch size is the only knob that
// controls how much you ask the model to look at in one request.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

type config struct {
	inputDir     string
	outputDir    string
	dpi          float64
	endpoint     string
	model        string
	apiKey       string
	prompt       string
	batchSize    int
	timeout      time.Duration
	maxRetries   int
	skipExisting bool
	stateDB      string // path to the sha512 state sqlite DB ("" = default ~/.goaiocr-state.sqlite3)
	force        bool   // reprocess files even if their hash is already in the state DB
	imageFormat  string // "png" or "jpeg" (page renders)
	jpegQuality  int
	keepTemp     bool
	verbose      bool
}

func main() {
	cfg := parseFlags()

	if err := run(cfg); err != nil {
		log.Fatalf("go-ai-ocr: %v", err)
	}
}

func parseFlags() config {
	var cfg config

	flag.StringVar(&cfg.inputDir, "i", "", "input directory to scan for PDFs and images (required)")
	flag.StringVar(&cfg.outputDir, "o", "", "output directory for final .md files + manifest (required)")
	flag.Float64Var(&cfg.dpi, "dpi", 450, "DPI used to rasterize PDF pages")
	flag.StringVar(&cfg.endpoint, "url", "http://127.0.0.1:11434/v1/chat/completions", "OpenAI-compatible chat completions endpoint (llama.cpp server)")
	flag.StringVar(&cfg.model, "model", "qwen2-vl", "model name to send in the request body")
	flag.StringVar(&cfg.apiKey, "api-key", os.Getenv("go-ai-ocr_API_KEY"), "bearer token for the AI endpoint, if required")
	flag.StringVar(&cfg.prompt, "prompt", defaultPrompt, "base instruction sent to the vision model")
	flag.IntVar(&cfg.batchSize, "batch-size", 2, "number of consecutive page images sent to the model per request")
	flag.DurationVar(&cfg.timeout, "timeout", 180*time.Second, "per-request timeout against the AI endpoint")
	flag.IntVar(&cfg.maxRetries, "max-retries", 2, "retries on transient AI endpoint failures")
	flag.BoolVar(&cfg.skipExisting, "skip-existing", true, "skip documents whose final .md already exists (resume support)")
	flag.StringVar(&cfg.stateDB, "state-db", defaultStateDBPath(), "path to the sha512 state sqlite DB (default ~/.goaiocr-state.sqlite3)")
	flag.BoolVar(&cfg.force, "force", false, "process files even if their hash is already in the state DB")
	flag.StringVar(&cfg.imageFormat, "image-format", "png", "format used for rasterized PDF pages: png or jpeg")
	flag.IntVar(&cfg.jpegQuality, "jpeg-quality", 92, "JPEG quality when -image-format=jpeg")
	flag.BoolVar(&cfg.keepTemp, "keep-temp", false, "keep rendered page images and per-batch Markdown instead of deleting them after each document")
	flag.BoolVar(&cfg.verbose, "v", false, "verbose logging")
	flag.Parse()

	if cfg.inputDir == "" || cfg.outputDir == "" {
		fmt.Fprintln(os.Stderr, "usage: go-ai-ocr -input <dir> -output <dir> [flags]")
		flag.PrintDefaults()
		os.Exit(2)
	}
	if cfg.batchSize < 1 {
		cfg.batchSize = 1
	}
	return cfg
}

const defaultPrompt = `You are an OCR and document-structuring engine. Read the attached page image(s) ` +
	`carefully and output clean, structured text suitable for a retrieval-augmented-generation ` +
	`(RAG) index. Rules:
- Transcribe all readable text faithfully, preserving reading order.
- Reconstruct headings, lists, and paragraphs using Markdown.
- Reconstruct tables using Markdown tables.
- Describe figures/charts/diagrams in a short bracketed caption, e.g. [Figure: bar chart showing ...].
- Do not add commentary, summaries, or anything not present on the page.
- Output only the resulting Markdown, nothing else.`

func run(cfg config) error {
	if err := os.MkdirAll(cfg.outputDir, 0o755); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}
	// Open the sha512 state DB (creates the file on first use). It lets a
	// later run skip content already processed and name same-content collisions apart.
	stateDB, err := newStateDB(cfg.stateDB)
	if err != nil {
		return fmt.Errorf("open state db: %w", err)
	}
	defer stateDB.Close()

	docs, err := discoverDocuments(cfg)
	if err != nil {
		return fmt.Errorf("discover input: %w", err)
	}
	if len(docs) == 0 {
		log.Println("no PDF or image files found under", cfg.inputDir)
		return nil
	}
	log.Printf("discovered %d document(s) to process (batch-size=%d)", len(docs), cfg.batchSize)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client := newAIClient(cfg)
	manifest := newManifestWriter(filepath.Join(cfg.outputDir, "manifest.jsonl"))
	defer manifest.Close()

	var done, failed, skipped int

	for _, doc := range docs {
		if ctx.Err() != nil {
			log.Println("interrupted, stopping before", doc.sourcePath)
			break
		}

		// Hash the raw input bytes. A repeat run of identical content
		// therefore hashes to the same digest, which is what lets us skip
		// cheaply and name genuine collisions apart.
		// Hash the raw input bytes. A repeat run of identical content
		// therefore hashes to the same digest, which is what lets us skip
		// cheaply and name genuine collisions apart.
		h, herr := fileHash512(doc.sourcePath)
		if herr != nil {
			log.Printf("skip %s: %v", doc.sourcePath, herr)
			continue
		}

		// Pick the output path first to check for existence.
		outPath, outSuf, oerr := stateDB.resolveOutputPath(doc.sourcePath, cfg.outputDir, h)
		if oerr != nil {
			log.Printf("output for %s: %v", doc.sourcePath, oerr)
			continue
		}

		// Skip content we have already processed, unless -force was set or the output file is missing.
		if !cfg.force {
			already, xerr := stateDB.hashExists(h)
			if xerr != nil {
				log.Printf("skip %s: state db: %v", doc.sourcePath, xerr)
				continue
			}

			if already {
				// If the file exists on disk, we can safely skip it. 
				// If it's missing, we reprocess to ensure consistency between DB and FS.
				exists := true
				if _, err := os.Stat(outPath); err != nil {
					exists = false
				}

				if exists {
					skipped++
					if cfg.verbose {
						log.Printf("SKIP  %s (hash %s already processed and output exists)", doc.sourcePath, shortHash(h))
					}
					manifest.WriteSkipped(doc, "")
					continue
				} else if cfg.verbose {
					log.Printf("REPROCESS  %s (hash %s in DB but output missing: %s)", doc.sourcePath, shortHash(h), outPath)
				}
			}
		}

		_ = outSuf
		if cfg.skipExisting {
			if _, err := os.Stat(outPath); err == nil {
				skipped++
				if cfg.verbose {
					log.Printf("SKIP  %s (already have %s)", doc.sourcePath, outPath)
				}
				manifest.WriteSkipped(doc, outPath)
				continue
			}
		}

		log.Printf("processing %s", doc.sourcePath)
		log.Printf("processing %s", doc.sourcePath)
		pageCount, err := processDocument(ctx, client, cfg, doc, outPath)
		if err != nil {
			failed++
			log.Printf("FAIL  %s: %v", doc.sourcePath, err)
			manifest.WriteFailure(doc, err)
			continue
		}
		done++
		// Record this processed source in the state DB so a later run
		// can skip content with the same hash. The upsert also updates the
		// collision cache so forced reprocesses agree with the persisted
		// output mapping.
		if uerr := stateDB.upsert(&stateRecord{
			SourcePath:  doc.sourcePath,
			Hash:        h,
			OutputPath:  outPath,
			SourceType:  doc.sourceType(),
			PageCount:   pageCount,
			DPI:         cfg.dpi,
			BatchSize:   cfg.batchSize,
			Model:       cfg.model,
			GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		}); uerr != nil {
			log.Printf("state db upsert for %s: %v", doc.sourcePath, uerr)
		}
		log.Printf("OK    %s (%d page(s)) -> %s", doc.sourcePath, pageCount, outPath)
		manifest.WriteSuccess(doc, outPath, pageCount)
	}

	log.Printf("done: %d succeeded, %d failed, %d skipped (output: %s)", done, failed, skipped, cfg.outputDir)
	if failed > 0 {
		return fmt.Errorf("%d document(s) failed, see log above and %s", failed, manifest.path)
	}
	return nil
}
