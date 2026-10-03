package main

// SQL drivers. Drop an import to add/remove a backend.
import (
	//	_ "github.com/lib/pq"  // "postgres"
	_ "modernc.org/sqlite" // "sqlite" (pure Go, no cgo)
)
