package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var extToMime = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".tif":  "image/tiff",
	".tiff": "image/tiff",
	".bmp":  "image/bmp",
	".webp": "image/webp",
	".gif":  "image/gif",
}

// processJob turns one job (a PDF page or a standalone image) into an image
// payload and submits it to the AI endpoint, returning the extracted text.
func processJob(ctx context.Context, client *aiClient, cfg config, j job) (string, error) {
	var (
		imgData  []byte
		mimeType string
		err      error
	)

	if j.isPDF {
		imgData, mimeType, err = renderPage(cfg, j)
		if err != nil {
			return "", fmt.Errorf("render: %w", err)
		}
	} else {
		imgData, err = os.ReadFile(j.sourcePath)
		if err != nil {
			return "", fmt.Errorf("read image: %w", err)
		}
		ext := strings.ToLower(filepath.Ext(j.sourcePath))
		mimeType = extToMime[ext]
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
	}

	text, err := client.Extract(ctx, imgData, mimeType)
	if err != nil {
		return "", fmt.Errorf("ai extract: %w", err)
	}
	return text, nil
}
