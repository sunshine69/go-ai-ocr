package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStateDBHashSkipAndCollision(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "sub", "state.sqlite3")

	db, err := newStateDB(dbPath)
	if err != nil {
		t.Fatalf("newStateDB: %v", err)
	}
	defer db.Close()

	// Verify the DB file was actually created on disk (default-path behavior).
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("state db file not created: %v", err)
	}

	hA := "aaaaaaa1111111111111111111111111"
	hB := "bbbbbb2222222222222222222222222"
	db.upsert(&stateRecord{SourcePath: "/in/doc.pdf", Hash: hA, OutputPath: "/out/doc.md", SourceType: "pdf", PageCount: 3})

	// hA exists -> should be skipped.
	exists, err := db.hashExists(hA)
	if err != nil || !exists {
		t.Fatalf("hashExists(hA) = %v, %v; want true, nil", exists, err)
	}
	// hB does not exist yet.
	exists, err = db.hashExists(hB)
	if err != nil || exists {
		t.Fatalf("hashExists(hB) = %v, %v; want false, nil", exists, err)
	}

	outDir := dir
	// Content A: plain doc.md does not exist yet, so A takes it.
	out1, suf1, err := db.resolveOutputPath("/in/doc.pdf", filepath.Join(outDir, "doc.md"), hA)
	if err != nil {
		t.Fatalf("resolveOutputPath(A): %v", err)
	}
	if suf1 != "" {
		t.Fatalf("suffix for A = %q; want empty", suf1)
	}
	if filepath.Base(out1) != "doc.md" {
		t.Fatalf("A output = %q; want doc.md", filepath.Base(out1))
	}
	// Persist A's output to disk so the next resolver sees it.
	if err := os.WriteFile(out1, []byte("content A"), 0o644); err != nil {
		t.Fatalf("write A output: %v", err)
	}

	// Content B: same name doc.md now exists on disk -> must be renamed.
	out2, suf2, err := db.resolveOutputPath("/in/doc.pdf", filepath.Join(outDir, "doc.md"), hB)
	if err != nil {
		t.Fatalf("resolveOutputPath(B): %v", err)
	}
	if suf2 == "" {
		t.Fatalf("suffix for B = %q; want non-empty", suf2)
	}
	if filepath.Base(out2) == "doc.md" {
		t.Fatalf("B output = %q; want a hash-suffixed name", filepath.Base(out2))
	}
	if suf2 != hB[:7] {
		t.Fatalf("B suffix = %q; want %q", suf2, hB[:7])
	}
	if filepath.Base(out1) == filepath.Base(out2) {
		t.Fatalf("colliding output base names: %q == %q", filepath.Base(out1), filepath.Base(out2))
	}
	t.Logf("A -> %s (suffix %q), B -> %s (suffix %q)", filepath.Base(out1), suf1, filepath.Base(out2), suf2)
}

func TestFileHash512(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	content := []byte("the quick brown fox")
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := fileHash512(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 128 {
		t.Fatalf("hash length = %d; want 128", len(h))
	}
	t.Logf("sha512=%s", h)
}
