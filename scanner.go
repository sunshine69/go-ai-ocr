package main

import (
	"os"
	"path/filepath"
	"strings"
)

// document describes one source file to process: a PDF (whose pages will
// all be rendered and folded into a single output .md) or a standalone
// image (treated as a one-page document).
type document struct {
	sourcePath string // absolute path to the original file
	relDir     string // directory of the source, relative to the input root
	baseName   string // source file name without extension
	isPDF      bool
}

var imageExts = map[string]bool{
	".png":  true,
	".jpg":  true,
	".jpeg": true,
	".tif":  true,
	".tiff": true,
	".bmp":  true,
	".webp": true,
	".gif":  true,
}

// discoverDocuments walks the input directory and returns one entry per
// PDF or image file found, in the order filepath.Walk visits them
// (alphabetical within each directory). Page expansion happens later,
// per document, during rendering.
func discoverDocuments(cfg config) ([]document, error) {
	var docs []document
	root := cfg.inputDir

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		relDir, rerr := filepath.Rel(root, filepath.Dir(path))
		if rerr != nil {
			relDir = "."
		}
		base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))

		switch {
		case ext == ".pdf":
			docs = append(docs, document{sourcePath: path, relDir: relDir, baseName: base, isPDF: true})
		case imageExts[ext]:
			docs = append(docs, document{sourcePath: path, relDir: relDir, baseName: base})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return docs, nil
}

func (d document) sourceType() string {
	if d.isPDF {
		return "pdf"
	}
	return "image"
}
