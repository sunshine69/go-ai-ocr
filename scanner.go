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

// discoverDocuments finds one entry per PDF or image file under the input.
// The input root may itself be a directory (in which case relDir preserves
// each file's sub-structure for nested outputs) or a single file (where the
// output .md must land directly inside -o). Page expansion happens later,
// per document, during rendering.
func discoverDocuments(cfg config) ([]document, error) {
	root := cfg.inputDir

	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}

	// Input is a single file (not a directory). The output .md must land
	// directly inside the output root (e.g. -o resources/parsed1 writes
	// resources/parsed1/<base>.md), so we treat the file as the input root
	// and set relDir = ".".
	if !info.IsDir() {
		return singleDoc(root), nil
	}

	// Input is a directory: walk it and tag each file with the path of its
	// parent directory relative to the input root so nested inputs map to
	// nested outputs. relDir defaults to "." for a top-level file.
	return discoverDir(root, root)
}

// discoverDir walks root (the input directory) collecting every PDF or
// image, tagging each with the directory of its source relative to root.
func discoverDir(root, walkPath string) ([]document, error) {
	var docs []document

	err := filepath.Walk(walkPath, func(path string, info os.FileInfo, err error) error {
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

		docs = append(docs, document{sourcePath: path, relDir: relDir, baseName: base, isPDF: ext == ".pdf"})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return docs, nil
}

// singleDoc builds the single document entry for a single-file input,
// with relDir = "." so resolveOutputPath writes into the output root.
func singleDoc(path string) []document {
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	return []document{{sourcePath: path, relDir: ".", baseName: base, isPDF: strings.EqualFold(filepath.Ext(path), ".pdf")}}
}

func (d document) sourceType() string {
	if d.isPDF {
		return "pdf"
	}
	return "image"
}
