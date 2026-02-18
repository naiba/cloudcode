package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Instance represents an opencode container instance.
type Instance struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	ContainerID string            `json:"container_id"`
	Status      string            `json:"status"` // created, running, stopped, error
	ErrorMsg    string            `json:"error_msg"`
	Port        int               `json:"port"`
	WorkDir     string            `json:"work_dir"`
	EnvVars     map[string]string `json:"env_vars"` // API keys, GH_TOKEN, etc.
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

// Store manages persistent storage of instances.
type Store struct {
	db *sql.DB
}

// New creates a new Store backed by SQLite.
func New(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0750); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	dbPath := filepath.Join(dataDir, "cloudcode.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	// Enable WAL mode for better concurrent access
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		return nil, fmt.Errorf("set WAL mode: %w", err)
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}

	return s, nil
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS instances (
			id           TEXT PRIMARY KEY,
			name         TEXT NOT NULL UNIQUE,
			container_id TEXT NOT NULL DEFAULT '',
			status       TEXT NOT NULL DEFAULT 'created',
			error_msg    TEXT NOT NULL DEFAULT '',
			port         INTEGER NOT NULL DEFAULT 0,
			work_dir     TEXT NOT NULL DEFAULT '/workspace',
			env_vars     TEXT NOT NULL DEFAULT '{}',
			created_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)
	`)
	if err != nil {
		return err
	}

	s.db.Exec(`ALTER TABLE instances ADD COLUMN error_msg TEXT NOT NULL DEFAULT ''`)
	return nil
}

// Create inserts a new instance.
func (s *Store) Create(inst *Instance) error {
	envJSON, err := json.Marshal(inst.EnvVars)
	if err != nil {
		return fmt.Errorf("marshal env vars: %w", err)
	}

	now := time.Now()
	inst.CreatedAt = now
	inst.UpdatedAt = now

	_, err = s.db.Exec(`
		INSERT INTO instances (id, name, container_id, status, error_msg, port, work_dir, env_vars, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, inst.ID, inst.Name, inst.ContainerID, inst.Status, inst.ErrorMsg, inst.Port, inst.WorkDir, string(envJSON), inst.CreatedAt, inst.UpdatedAt)
	if err != nil {
		return fmt.Errorf("insert instance: %w", err)
	}
	return nil
}

// Get retrieves an instance by ID.
func (s *Store) Get(id string) (*Instance, error) {
	row := s.db.QueryRow(`SELECT id, name, container_id, status, error_msg, port, work_dir, env_vars, created_at, updated_at FROM instances WHERE id = ?`, id)
	return scanInstance(row)
}

// GetByName retrieves an instance by name.
func (s *Store) GetByName(name string) (*Instance, error) {
	row := s.db.QueryRow(`SELECT id, name, container_id, status, error_msg, port, work_dir, env_vars, created_at, updated_at FROM instances WHERE name = ?`, name)
	return scanInstance(row)
}

// List returns all instances.
func (s *Store) List() ([]*Instance, error) {
	rows, err := s.db.Query(`SELECT id, name, container_id, status, error_msg, port, work_dir, env_vars, created_at, updated_at FROM instances ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("query instances: %w", err)
	}
	defer rows.Close()

	var instances []*Instance
	for rows.Next() {
		inst, err := scanInstanceRow(rows)
		if err != nil {
			return nil, err
		}
		instances = append(instances, inst)
	}
	return instances, rows.Err()
}

// Update updates an instance.
func (s *Store) Update(inst *Instance) error {
	envJSON, err := json.Marshal(inst.EnvVars)
	if err != nil {
		return fmt.Errorf("marshal env vars: %w", err)
	}

	inst.UpdatedAt = time.Now()

	_, err = s.db.Exec(`
		UPDATE instances SET name=?, container_id=?, status=?, error_msg=?, port=?, work_dir=?, env_vars=?, updated_at=?
		WHERE id=?
	`, inst.Name, inst.ContainerID, inst.Status, inst.ErrorMsg, inst.Port, inst.WorkDir, string(envJSON), inst.UpdatedAt, inst.ID)
	if err != nil {
		return fmt.Errorf("update instance: %w", err)
	}
	return nil
}

// Delete removes an instance by ID.
func (s *Store) Delete(id string) error {
	_, err := s.db.Exec(`DELETE FROM instances WHERE id = ?`, id)
	return err
}

// Close closes the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// scanInstance scans a single row into an Instance.
func scanInstance(row *sql.Row) (*Instance, error) {
	var inst Instance
	var envJSON string
	if err := row.Scan(&inst.ID, &inst.Name, &inst.ContainerID, &inst.Status, &inst.ErrorMsg, &inst.Port, &inst.WorkDir, &envJSON, &inst.CreatedAt, &inst.UpdatedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(envJSON), &inst.EnvVars); err != nil {
		return nil, fmt.Errorf("unmarshal env vars: %w", err)
	}
	return &inst, nil
}

// scanInstanceRow scans from sql.Rows.
func scanInstanceRow(rows *sql.Rows) (*Instance, error) {
	var inst Instance
	var envJSON string
	if err := rows.Scan(&inst.ID, &inst.Name, &inst.ContainerID, &inst.Status, &inst.ErrorMsg, &inst.Port, &inst.WorkDir, &envJSON, &inst.CreatedAt, &inst.UpdatedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(envJSON), &inst.EnvVars); err != nil {
		return nil, fmt.Errorf("unmarshal env vars: %w", err)
	}
	return &inst, nil
}
