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
	out1, suf1, err := db.resolveOutputPath("/in/doc.pdf", outDir, hA)
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
	out2, suf2, err := db.resolveOutputPath("/in/doc.pdf", outDir, hB)
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

// skipDecision mirrors the run(main.go) logic for one document: it decides
// whether a document should be skipped given the hash, the state DB, the
// on-disk output file, and the -force flag. Kept as a test helper so the
// exact skip semantics are covered by an automated test.
//
// -force short-circuits every skip path: a forced reprocess always runs.
func skipDecision(db *StateDB, sourcePath, outRoot string, h string, force, skipExisting bool) bool {
	if force {
		return false
	}
	already, xerr := db.hashExists(h)
	if xerr != nil {
		return true // skip on DB error
	}
	if already {
		outPath, oerr := db.outputPathFor(h)
		if oerr != nil {
			return true
		}
		if outPath != "" {
			if _, statErr := os.Stat(outPath); statErr == nil {
				return true
			}
		}
	}
	outPath, _, oerr := db.resolveOutputPath(sourcePath, outRoot, h)
	if oerr != nil {
		return true
	}
	if skipExisting {
		if _, statErr := os.Stat(outPath); statErr == nil {
			return true
		}
	}
	return false
}

func TestStateDBSkipWorkflow(t *testing.T) {
	dir := t.TempDir()
	db, err := newStateDB(filepath.Join(dir, "sub", "state.sqlite3"))
	if err != nil {
		t.Fatalf("newStateDB: %v", err)
	}
	defer db.Close()

	src := "/in/doc.pdf"
	outRoot := filepath.Join(dir, "out")
	hA := "aaaaaaa1111111111111111111111111"

	// First run: hash not recorded, output missing -> NOT skipped.
	if skipDecision(db, src, outRoot, hA, false, true) != false {
		t.Fatalf("first run: expected not-skipped")
	}

	// Simulate processing: resolve output, write it, record in DB.
	out1, _, err := db.resolveOutputPath(src, outRoot, hA)
	if err != nil {
		t.Fatalf("resolveOutputPath: %v", err)
	}
	if err = os.MkdirAll(filepath.Dir(out1), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err = os.WriteFile(out1, []byte("content A"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err = db.upsert(&stateRecord{SourcePath: src, Hash: hA, OutputPath: out1, SourceType: "pdf", PageCount: 1}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Second run: hash recorded + output present -> skipped.
	if skipDecision(db, src, outRoot, hA, false, true) != true {
		t.Fatalf("second run: expected skipped")
	}

	// -force reprocess: NOT skipped.
	if skipDecision(db, src, outRoot, hA, true, true) != false {
		t.Fatalf("-force: expected not-skipped")
	}

	// Missing output but hash present -> reprocess (not skipped).
	if err = os.Remove(out1); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if skipDecision(db, src, outRoot, hA, false, true) != false {
		t.Fatalf("output missing: expected not-skipped")
	}
}

// TestStateDBTwoSameNameDifferentContent verifies two same-named inputs that
// hash differently both resolve to distinct output paths, and that each
// recorded output can be looked up independently. It mirrors the run flow:
// A resolves, then upserts (populating the in-memory outputHashes map),
// only then does B resolve against that cache and collide on the basename.
func TestStateDBTwoSameNameDifferentContent(t *testing.T) {
	dir := t.TempDir()
	db, err := newStateDB(filepath.Join(dir, "state.sqlite3"))
	if err != nil {
		t.Fatalf("newStateDB: %v", err)
	}
	defer db.Close()

	outRoot := filepath.Join(dir, "out")
	dirA := filepath.Join(dir, "a")
	dirB := filepath.Join(dir, "b")
	hA := "aaaaaaa1111111111111111111111111"
	hB := "bbbbbb2222222222222222222222222"

	// Two source files in different dirs, same base name.
	srcA := filepath.Join(dirA, "doc.pdf")
	srcB := filepath.Join(dirB, "doc.pdf")

	// A: no state yet -> plain doc.md. Persist to disk, then upsert so the
	// in-memory outputHashes cache records A before B resolves.
	outA, _, err := db.resolveOutputPath(srcA, outRoot, hA)
	if err != nil {
		t.Fatalf("resolveOutputPath(A): %v", err)
	}
	if err = os.MkdirAll(filepath.Dir(outA), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err = os.WriteFile(outA, []byte("content A"), 0o644); err != nil {
		t.Fatalf("write A: %v", err)
	}
	if err = db.upsert(&stateRecord{SourcePath: srcA, Hash: hA, OutputPath: outA, SourceType: "pdf", PageCount: 1}); err != nil {
		t.Fatalf("upsert A: %v", err)
	}

	// B: resolve now. A's upsert recorded doc.md -> hA, so B collides and
	// must get a hash-suffixed name instead of clobbering A's output.
	outB, _, err := db.resolveOutputPath(srcB, outRoot, hB)
	if err != nil {
		t.Fatalf("resolveOutputPath(B): %v", err)
	}
	if err = db.upsert(&stateRecord{SourcePath: srcB, Hash: hB, OutputPath: outB, SourceType: "pdf", PageCount: 1}); err != nil {
		t.Fatalf("upsert B: %v", err)
	}

	if filepath.Base(outA) == filepath.Base(outB) {
		t.Fatalf("expected distinct output basenames, got %q and %q", filepath.Base(outA), filepath.Base(outB))
	}
	// resolveOutputPath joins base + hash[:7] + ".md" with no separator.
	wantBase := "doc" + hB[:7] + ".md"
	if filepath.Base(outA) != "doc.md" {
		t.Fatalf("A output = %q; want doc.md", filepath.Base(outA))
	}
	if filepath.Base(outB) != wantBase {
		t.Fatalf("B output = %q; want %q", filepath.Base(outB), wantBase)
	}
	// Each hash resolves back to its own output.
	backA, err := db.outputPathFor(hA)
	if err != nil || backA != outA {
		t.Fatalf("outputPathFor(A) = %q, %v; want %q", backA, err, outA)
	}
	backB, err := db.outputPathFor(hB)
	if err != nil || backB != outB {
		t.Fatalf("outputPathFor(B) = %q, %v; want %q", backB, err, outB)
	}
	t.Logf("A -> %s, B -> %s", filepath.Base(outA), filepath.Base(outB))
}
