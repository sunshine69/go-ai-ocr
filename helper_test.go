package main

import "os"

// testPdf writes a small non-empty file at path so a single-file input
// can be tested end-to-end. The content need not be a valid PDF for
// discoverDocuments, which only inspects the file's path and extension.
func testPdf(path string) (*os.File, error) {
	if err := os.WriteFile(path, []byte("not a real pdf"), 0o644); err != nil {
		return nil, err
	}
	return os.Open(path)
}
