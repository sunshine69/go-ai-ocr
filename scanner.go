package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// job describes a single unit of work: one PDF page, or one standalone image.
type job struct {
	sourcePath string // absolute path to the original file on disk
	relDir     string // directory of the source, relative to the input root
	baseName   string // source file name without extension
	isPDF      bool
	pageNum    int // 1-based page number; 0 for standalone images
	pageCount  int // total pages in the source PDF; 0 for standalone images
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

// discoverJobs walks the input directory. PDFs are expanded into one job per
// page (page count is discovered lazily during rendering since opening the
// document twice is wasteful); this pass just enumerates source files, and
// expandPDF below turns each PDF file into its page jobs.
func discoverJobs(cfg config) ([]job, error) {
	var files []job
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
			pages, perr := expandPDF(path, relDir, base)
			if perr != nil {
				return fmt.Errorf("inspect %s: %w", path, perr)
			}
			files = append(files, pages...)
		case imageExts[ext]:
			files = append(files, job{
				sourcePath: path,
				relDir:     relDir,
				baseName:   base,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

func (j job) describe() string {
	if j.isPDF {
		return fmt.Sprintf("%s (page %d/%d)", j.sourcePath, j.pageNum, j.pageCount)
	}
	return j.sourcePath
}

// outputPath returns where the extracted text for this job should be written,
// mirroring the input directory structure under the output root.
func (j job) outputPath(outputRoot string) string {
	dir := filepath.Join(outputRoot, j.relDir, j.baseName)
	if j.isPDF {
		return filepath.Join(dir, fmt.Sprintf("%s_page_%04d.md", j.baseName, j.pageNum))
	}
	return filepath.Join(dir, j.baseName+".md")
}
