package main

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"

	fitz "github.com/gen2brain/go-fitz"
)

// renderPDFPages opens path, rasterizes every page at cfg.dpi, and writes
// each page to its own image file inside tempDir. It returns the page
// image paths in page order. Everything happens in this one call, on the
// caller's goroutine — there is no concurrency here.
func renderPDFPages(cfg config, srcPath, tempDir string) ([]string, error) {
	doc, err := fitz.New(srcPath)
	if err != nil {
		return nil, fmt.Errorf("open pdf: %w", err)
	}
	defer doc.Close()

	n := doc.NumPage()
	if n <= 0 {
		return nil, fmt.Errorf("pdf has no pages")
	}

	ext := ".png"
	if cfg.imageFormat == "jpeg" || cfg.imageFormat == "jpg" {
		ext = ".jpg"
	}

	pagePaths := make([]string, 0, n)
	for i := 0; i < n; i++ {
		img, err := doc.ImageDPI(i, cfg.dpi)
		if err != nil {
			return nil, fmt.Errorf("rasterize page %d at %.0f dpi: %w", i+1, cfg.dpi, err)
		}
		data, _, err := encodeImage(img, cfg)
		if err != nil {
			return nil, fmt.Errorf("encode page %d: %w", i+1, err)
		}
		p := filepath.Join(tempDir, fmt.Sprintf("page_%04d%s", i+1, ext))
		if err := os.WriteFile(p, data, 0o644); err != nil {
			return nil, fmt.Errorf("write page %d: %w", i+1, err)
		}
		pagePaths = append(pagePaths, p)
	}
	return pagePaths, nil
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
