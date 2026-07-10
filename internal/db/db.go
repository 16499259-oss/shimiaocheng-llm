// Package db provides SQLite database initialization and connection management.
package db

import (
	"database/sql"
	"fmt"
	"log"
	"time"

	_ "modernc.org/sqlite"
)

// DB wraps the SQLite database connection.
type DB struct {
	Conn *sql.DB
}

// Open initializes a SQLite database connection with WAL mode enabled.
func Open(dbPath string) (*DB, error) {
	// Enable WAL mode via pragma in the DSN
	dsn := fmt.Sprintf("%s?_journal_mode=WAL&_busy_timeout=5000&_synchronous=NORMAL&_cache_size=-20000", dbPath)

	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Configure connection pool
	conn.SetMaxOpenConns(1)    // SQLite only supports one writer at a time
	conn.SetMaxIdleConns(1)
	conn.SetConnMaxLifetime(0) // Connections are persistent

	// Verify connection
	if err := conn.Ping(); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	// Additional pragmas for performance
	pragmas := []string{
		"PRAGMA foreign_keys = ON",
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = NORMAL",
		"PRAGMA cache_size = -20000",
		"PRAGMA busy_timeout = 5000",
	}

	for _, p := range pragmas {
		if _, err := conn.Exec(p); err != nil {
			log.Printf("warning: pragma %s: %v", p, err)
		}
	}

	log.Printf("Database opened successfully: %s (WAL mode)", dbPath)
	return &DB{Conn: conn}, nil
}

// Close gracefully closes the database connection.
func (d *DB) Close() error {
	if d.Conn != nil {
		return d.Conn.Close()
	}
	return nil
}

// Now returns the current time in RFC3339 format for DB consistency.
func Now() string {
	return time.Now().Format(time.RFC3339)
}
