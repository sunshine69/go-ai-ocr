// Command go-ai-ocr scans a directory for PDFs and images, renders PDF pages
// to high-DPI images via MuPDF (go-fitz), sends each page/image to a
// vision-capable model served by llama.cpp (Qwen2-VL, OpenAI-compatible
// /v1/chat/completions), and writes the resulting structured text to an
// output directory in a layout a RAG indexer can consume directly.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sync"
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
	concurrency  int
	timeout      time.Duration
	maxRetries   int
	skipExisting bool
	imageFormat  string // "png" or "jpeg" (page renders)
	jpegQuality  int
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

	flag.StringVar(&cfg.inputDir, "input", "", "input directory to scan for PDFs and images (required)")
	flag.StringVar(&cfg.outputDir, "output", "", "output directory for extracted text + manifest (required)")
	flag.Float64Var(&cfg.dpi, "dpi", 450, "DPI used to rasterize PDF pages")
	flag.StringVar(&cfg.endpoint, "endpoint", "http://127.0.0.1:8080/v1/chat/completions", "OpenAI-compatible chat completions endpoint (llama.cpp server)")
	flag.StringVar(&cfg.model, "model", "qwen2-vl", "model name to send in the request body")
	flag.StringVar(&cfg.apiKey, "api-key", os.Getenv("go-ai-ocr_API_KEY"), "bearer token for the AI endpoint, if required")
	flag.StringVar(&cfg.prompt, "prompt", defaultPrompt, "instruction sent to the vision model alongside each image")
	flag.IntVar(&cfg.concurrency, "concurrency", runtime.NumCPU(), "number of pages/images processed concurrently")
	flag.DurationVar(&cfg.timeout, "timeout", 180*time.Second, "per-request timeout against the AI endpoint")
	flag.IntVar(&cfg.maxRetries, "max-retries", 2, "retries on transient AI endpoint failures")
	flag.BoolVar(&cfg.skipExisting, "skip-existing", true, "skip pages whose output file already exists (resume support)")
	flag.StringVar(&cfg.imageFormat, "image-format", "png", "format used for rasterized PDF pages: png or jpeg")
	flag.IntVar(&cfg.jpegQuality, "jpeg-quality", 92, "JPEG quality when -image-format=jpeg")
	flag.BoolVar(&cfg.verbose, "v", false, "verbose logging")
	flag.Parse()

	if cfg.inputDir == "" || cfg.outputDir == "" {
		fmt.Fprintln(os.Stderr, "usage: go-ai-ocr -input <dir> -output <dir> [flags]")
		flag.PrintDefaults()
		os.Exit(2)
	}
	return cfg
}

const defaultPrompt = `You are an OCR and document-structuring engine. Read the attached page image ` +
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

	jobs, err := discoverJobs(cfg)
	if err != nil {
		return fmt.Errorf("discover input: %w", err)
	}
	if len(jobs) == 0 {
		log.Println("no PDF or image files found under", cfg.inputDir)
		return nil
	}
	log.Printf("discovered %d page(s)/image(s) to process", len(jobs))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client := newAIClient(cfg)
	manifest := newManifestWriter(filepath.Join(cfg.outputDir, "manifest.jsonl"))
	defer manifest.Close()

	sem := make(chan struct{}, max(1, cfg.concurrency))
	var wg sync.WaitGroup
	var mu sync.Mutex
	var failed, done, skipped int

	for _, j := range jobs {
		j := j
		outPath := j.outputPath(cfg.outputDir)

		if cfg.skipExisting {
			if _, err := os.Stat(outPath); err == nil {
				mu.Lock()
				skipped++
				mu.Unlock()
				manifest.WriteSkipped(j, outPath)
				continue
			}
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()

			if ctx.Err() != nil {
				return
			}

			text, procErr := processJob(ctx, client, cfg, j)
			mu.Lock()
			defer mu.Unlock()
			if procErr != nil {
				failed++
				log.Printf("FAIL  %s: %v", j.describe(), procErr)
				manifest.WriteFailure(j, procErr)
				return
			}
			if err := writeOutput(outPath, j, cfg, text); err != nil {
				failed++
				log.Printf("FAIL  %s: write output: %v", j.describe(), err)
				manifest.WriteFailure(j, err)
				return
			}
			done++
			if cfg.verbose {
				log.Printf("OK    %s -> %s", j.describe(), outPath)
			}
			manifest.WriteSuccess(j, outPath)
		}()
	}

	wg.Wait()

	log.Printf("done: %d succeeded, %d failed, %d skipped (output: %s)", done, failed, skipped, cfg.outputDir)
	if failed > 0 {
		return fmt.Errorf("%d job(s) failed, see log above and %s", failed, manifest.path)
	}
	return nil
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
