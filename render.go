package main

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"

	fitz "github.com/gen2brain/go-fitz"
)

// expandPDF opens path just long enough to read its page count, then returns
// one job per page. The document is not kept open: rendering happens later,
// on demand, per page (see renderPage), so concurrent workers never share a
// single go-fitz Document, which is not safe for concurrent use.
func expandPDF(path, relDir, base string) ([]job, error) {
	doc, err := fitz.New(path)
	if err != nil {
		return nil, fmt.Errorf("open pdf: %w", err)
	}
	defer doc.Close()

	n := doc.NumPage()
	if n <= 0 {
		return nil, fmt.Errorf("pdf has no pages")
	}

	jobs := make([]job, 0, n)
	for p := 1; p <= n; p++ {
		jobs = append(jobs, job{
			sourcePath: path,
			relDir:     relDir,
			baseName:   base,
			isPDF:      true,
			pageNum:    p,
			pageCount:  n,
		})
	}
	return jobs, nil
}

// renderPage rasterizes a single PDF page to an encoded image (PNG or JPEG)
// at cfg.dpi. It opens and closes its own Document handle so it is safe to
// call from multiple goroutines concurrently.
func renderPage(cfg config, j job) (data []byte, mimeType string, err error) {
	doc, err := fitz.New(j.sourcePath)
	if err != nil {
		return nil, "", fmt.Errorf("open pdf: %w", err)
	}
	defer doc.Close()

	img, err := doc.ImageDPI(j.pageNum-1, cfg.dpi)
	if err != nil {
		return nil, "", fmt.Errorf("rasterize page %d at %.0f dpi: %w", j.pageNum, cfg.dpi, err)
	}

	return encodeImage(img, cfg)
}

func encodeImage(img image.Image, cfg config) (data []byte, mimeType string, err error) {
	var buf bytes.Buffer
	switch cfg.imageFormat {
	case "jpeg", "jpg":
		q := cfg.jpegQuality
		if q <= 0 || q > 100 {
			q = 92
		}
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: q}); err != nil {
			return nil, "", fmt.Errorf("encode jpeg: %w", err)
		}
		return buf.Bytes(), "image/jpeg", nil
	default:
		if err := png.Encode(&buf, img); err != nil {
			return nil, "", fmt.Errorf("encode png: %w", err)
		}
		return buf.Bytes(), "image/png", nil
	}
}
