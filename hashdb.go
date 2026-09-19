package main

import (
	"crypto/sha512"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	// cgo-free SQLite driver (pure-Go port of the C SQLite3 library). The
	// rest of the tool already links CGO via go-fitz, but keeping this driver
	// cgo-free means the state DB itself adds no further toolchain coupling.
	_ "modernc.org/sqlite"
)

// stateRecord is one row in the processed-files ledger: the source file
// that was hashed, its sha512 (hex), and where its Markdown landed.
type stateRecord struct {
	SourcePath  string
	Hash        string
	OutputPath  string
	SourceType  string
	PageCount   int
	DPI         float64
	BatchSize   int
	Model       string
	GeneratedAt string
}

const stateSchema = `
CREATE TABLE IF NOT EXISTS processed_files (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    file_path     TEXT    NOT NULL,
    hash          TEXT    NOT NULL,
    output_path   TEXT    NOT NULL DEFAULT '',
    source_type   TEXT    NOT NULL DEFAULT '',
    page_count    INTEGER NOT NULL DEFAULT 0,
    dpi           REAL    NOT NULL DEFAULT 0,
    batch_size    INTEGER NOT NULL DEFAULT 0,
    model         TEXT    NOT NULL DEFAULT '',
    generated_at  TEXT    NOT NULL DEFAULT '',
    created_at    TEXT    NOT NULL,
    updated_at    TEXT    NOT NULL
);
-- Content is unique: a given sha512 maps to exactly one record.
CREATE UNIQUE INDEX IF NOT EXISTS idx_processed_files_hash
    ON processed_files(hash);
-- Lets us detect a same-name/different-content output collision quickly.
CREATE INDEX IF NOT EXISTS idx_processed_files_output
    ON processed_files(output_path);
`

// StateDB is a small SQLite ledger of every source file the tool has
// hashed and where its output was written. It exists so a later run can
// skip content it has already processed (by sha512) and flag same-named
// outputs that now hold different content.
type StateDB struct {
	db *sql.DB
	// collisionCache mirrors the hashes that have produced each output
	// filename. It is kept in sync with the ledger (and upsert below) so a
	// forced reprocess of the current file does not flag itself as a
	// name-collision.
	collisionCache map[string]map[string]bool
	path           string
}

// newStateDB opens (creating if needed) the SQLite ledger at path and
// newStateDB opens (creating if needed) the SQLite ledger at path and
// ensures the schema exists.
func newStateDB(path string) (*StateDB, error) {
	if path != "" && path != ":memory:" {
		if dir := filepath.Dir(path); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("create state db dir: %w", err)
			}
		}
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open state db: %w", err)
	}
	// SQLite tolerates a pool, but writes are serialized anyway; keep it
	// simple and avoid "database is locked" races on a single-writer ledger.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(stateSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init state db: %w", err)
	}
	sd := &StateDB{db: db, path: path}
	cached, cerr := sd.collisionNameHashes()
	if cerr != nil {
		db.Close()
		return nil, fmt.Errorf("load collision cache: %w", cerr)
	}
	sd.collisionCache = cached
	return sd, nil
}

// ensures the schema exists.
func (db *StateDB) Close() {
	if db != nil {
		if err := db.db.Close(); err != nil {
			log.Printf("go-ai-ocr: closing state db: %v", err)
		}
	}
}

// hashExists reports whether a source with the given sha512 (hex) has
// already been recorded in the ledger.
func (db *StateDB) hashExists(hash string) (bool, error) {
	var file sql.NullString
	err := db.db.QueryRow(`SELECT file_path FROM processed_files WHERE hash = ?`, hash).Scan(&file)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// upsert records (or refreshes the output mapping for) a processed source.
// A row is inserted for new content; for already-seen content (matching
// hash) only the output/path bookkeeping is updated, so a forced reprocess
// does not create a duplicate row.
func (db *StateDB) upsert(rec *stateRecord) error {
	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := db.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(
		`INSERT INTO processed_files (file_path, hash, output_path, source_type, page_count, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(hash) DO UPDATE SET
		     output_path  = excluded.output_path,
		     source_type  = excluded.source_type,
		     page_count   = excluded.page_count,
		     updated_at   = excluded.updated_at`,
		rec.SourcePath, rec.Hash, rec.OutputPath, rec.SourceType, rec.PageCount, now, now,
	); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// Keep the in-memory collision cache consistent with the write so a
	// forced reprocess of the current file does not flag itself.
	db.rememberOutput(rec.OutputPath, rec.Hash)
	return nil
}

// rememberOutput records (in the in-memory cache) that hash has ever been
// produced at outputPath, so resolveOutputPath agrees with the persisted
// ledger.
func (db *StateDB) rememberOutput(outputPath, hash string) {
	if outputPath == "" || hash == "" {
		return
	}
	if db.collisionCache == nil {
		db.collisionCache = make(map[string]map[string]bool)
	}
	name := filepath.Base(outputPath)
	if db.collisionCache[name] == nil {
		db.collisionCache[name] = make(map[string]bool)
	}
	db.collisionCache[name][hash] = true
}

// resolveOutputPath returns the output markdown path for source, whose
// content hashes to sourceHash. It defaults to <base>.md (derived from the
// source's file name) and, if that path already exists on disk, falls back
// to <base>_<first 7 hex chars of hash>.md so two same-named but
// different-content outputs never clobber each other.

func (db *StateDB) resolveOutputPath(source, outputPath, sourceHash string) (path, suffix string, err error) {
	base := strings.TrimSuffix(filepath.Base(source), filepath.Ext(source))
	plain := filepath.Join(filepath.Dir(outputPath), base+".md")

	if _, statErr := os.Stat(plain); statErr != nil {
		return plain, "", nil
	}
	suffixed := base + sourceHash[:7] + ".md"
	return suffixed, sourceHash[:7], nil
}

// outputNameHashes returns a map from output filename (e.g. "notes.md")
// to the set of distinct hashes that have ever produced it. This is used
// to detect same-named outputs that hold different content, which can span
// multiple runs since the ledger persists.
func (db *StateDB) outputNameHashes() (map[string]map[string]bool, error) {
	rows, err := db.db.Query(`SELECT output_path, hash FROM processed_files WHERE output_path != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	byName := make(map[string]map[string]bool)
	for rows.Next() {
		var outPath, hash string
		if err := rows.Scan(&outPath, &hash); err != nil {
			return nil, err
		}
		name := filepath.Base(outPath)
		if byName[name] == nil {
			byName[name] = make(map[string]bool)
		}
		byName[name][hash] = true
	}
	return byName, rows.Err()
}

// collisionNameHashes loads the persisted output->hash map into the in-memory
// collision cache so resolveOutputPath stays consistent with the ledger
// across runs.
func (db *StateDB) collisionNameHashes() (map[string]map[string]bool, error) {
	byName, err := db.outputNameHashes()
	if err != nil {
		return nil, err
	}
	if db.collisionCache == nil {
		db.collisionCache = make(map[string]map[string]bool)
	}
	for name, hashes := range byName {
		db.collisionCache[name] = hashes
	}
	return db.collisionCache, nil
}

// fileHash512 returns the lowercase hex sha512 digest of the file at path.
func fileHash512(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open: %w", err)
	}
	defer f.Close()

	h := sha512.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// defaultStateDBPath returns the default path for the sha512 state ledger:
// go-ai-ocr-state.sqlite3 in the user's home directory. If the home
// directory cannot be resolved, it falls back to a working-directory path.
func defaultStateDBPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".goaiocr-state.sqlite3"
	}
	return filepath.Join(home, ".goaiocr-state.sqlite3")
}

// shortHash returns a compact, human-friendly rendering of a (long) hex
// digest for log lines.
func shortHash(h string) string {
	const n = 8
	if len(h) <= n {
		return h
	}
	return h[:n] + "..."
}
