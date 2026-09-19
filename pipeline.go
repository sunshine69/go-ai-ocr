package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// processDocument runs the whole pipeline for one document, one step at a
// time, with no concurrency:
//  1. Render PDF pages to temp image files (skipped for standalone images).
//  2. Split the page list into fixed-size batches (cfg.batchSize).
//  3. For each batch, in order: base64-encode its images, submit one
//     blocking request to the model, save the returned Markdown as a part
//     file.
//  4. Concatenate all part files, in page order, into the final .md.
//  5. Delete the temp dir (rendered images + part files) unless -keep-temp.
//
// It returns the number of pages processed.
func processDocument(ctx context.Context, client *aiClient, cfg config, doc document, outPath string) (int, error) {
	tempDir, err := os.MkdirTemp("", "go-ai-ocr-*")
	if err != nil {
		return 0, fmt.Errorf("create temp dir: %w", err)
	}
	cleanupTemp := func() {
		if !cfg.keepTemp {
			os.RemoveAll(tempDir)
		} else if cfg.verbose {
			fmt.Printf("kept temp dir for %s: %s\n", doc.sourcePath, tempDir)
		}
	}

	var pagePaths []string
	if doc.isPDF {
		pagePaths, err = renderPDFPages(cfg, doc.sourcePath, tempDir)
		if err != nil {
			cleanupTemp()
			return 0, fmt.Errorf("render pages: %w", err)
		}
	} else {
		pagePaths = []string{doc.sourcePath}
	}

	batches := chunkPaths(pagePaths, cfg.batchSize)
	partPaths := make([]string, 0, len(batches))

	for i, batch := range batches {
		if ctx.Err() != nil {
			// Leave the temp dir behind so nothing already fetched is lost;
			// the manifest/error makes clear this document is incomplete.
			return len(pagePaths), fmt.Errorf("interrupted before batch %d/%d", i+1, len(batches))
		}
		if cfg.verbose {
			fmt.Printf("  batch %d/%d (%d page(s)) -> %s\n", i+1, len(batches), len(batch), cfg.endpoint)
		}

		text, err := client.ExtractBatch(ctx, batch)
		if err != nil {
			// Keep the temp dir on failure regardless of -keep-temp: it has
			// the rendered pages and whatever batches already succeeded,
			// which is exactly what you want when retrying by hand.
			return len(pagePaths), fmt.Errorf("batch %d/%d (pages %s): %w", i+1, len(batches), batchLabel(batch), err)
		}

		partPath := filepath.Join(tempDir, fmt.Sprintf("part_%04d.md", i+1))
		if err := os.WriteFile(partPath, []byte(text), 0o644); err != nil {
			return len(pagePaths), fmt.Errorf("write batch %d/%d output: %w", i+1, len(batches), err)
		}
		partPaths = append(partPaths, partPath)
	}

	if err := writeFinalDoc(outPath, doc, cfg, len(pagePaths), partPaths); err != nil {
		return len(pagePaths), fmt.Errorf("write final document: %w", err)
	}

	cleanupTemp()
	return len(pagePaths), nil
}

// chunkPaths splits paths into consecutive groups of at most size.
func chunkPaths(paths []string, size int) [][]string {
	if size < 1 {
		size = 1
	}
	var batches [][]string
	for i := 0; i < len(paths); i += size {
		end := i + size
		if end > len(paths) {
			end = len(paths)
		}
		batches = append(batches, paths[i:end])
	}
	return batches
}

func batchLabel(batch []string) string {
	if len(batch) == 1 {
		return filepath.Base(batch[0])
	}
	return fmt.Sprintf("%s..%s", filepath.Base(batch[0]), filepath.Base(batch[len(batch)-1]))
}
