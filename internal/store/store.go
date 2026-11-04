// Package store persists employees and their face templates in SQLite.
//
// The schema mirrors the original detection-server registry so an existing
// face database file can be adopted as-is: employees carry the profile, and
// every face_templates row holds an SFace embedding (little-endian float32
// blob) plus the enrollment image the embedding was computed from.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Sentinel errors callers can match on.
var (
	// ErrNotFound marks a missing employee or template.
	ErrNotFound = errors.New("not found")
	// ErrDuplicateEmployeeNo marks a unique employee_no collision.
	ErrDuplicateEmployeeNo = errors.New("employee_no already exists")
	// ErrInvalidEmbedding marks non-finite or empty embedding values.
	ErrInvalidEmbedding = errors.New("embedding must contain finite values")
	// ErrDuplicateUsername marks a unique username collision.
	ErrDuplicateUsername = errors.New("username already exists")
)

// Employee is one staff profile row.
type Employee struct {
	ID          int64  `json:"id"`
	EmployeeNo  string `json:"employee_no"`
	Name        string `json:"name"`
	Department  string `json:"department"`
	Enabled     bool   `json:"enabled"`
	SampleCount int    `json:"sample_count"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

// Template is one enrolled face sample.
type Template struct {
	ID            int64   `json:"id"`
	EmployeeID    int64   `json:"employee_id"`
	EmbeddingDim  int     `json:"embedding_dim"`
	HasImage      bool    `json:"has_image"`
	ImageMimeType string  `json:"image_mime_type"`
	ModelVersion  string  `json:"model_version"`
	QualityScore  float64 `json:"quality_score"`
	CreatedAt     string  `json:"created_at"`

	// Embedding is the little-endian float32 blob; empty for image-only
	// legacy rows.
	Embedding []byte
	// Image is the original enrollment image bytes, present for new rows.
	Image []byte
}

// Enrollment carries what the engine produced for one uploaded sample.
type Enrollment struct {
	Embedding     []float32
	ModelVersion  string
	QualityScore  float64
	FaceBBox      [4]float32
	Image         []byte
	ImageMimeType string
}

// MatchTemplate is the recognizer-facing projection of a template.
type MatchTemplate struct {
	TemplateID   int64
	EmployeeID   int64
	EmployeeNo   string
	Name         string
	Embedding    []float32
	ModelVersion string
}

// Store wraps the SQLite database.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the database file.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, fmt.Errorf("opening database %s: %w", path, err)
	}
	// modernc/sqlite serializes writes; one connection avoids SQLITE_BUSY
	db.SetMaxOpenConns(1)
	store := &Store{db: db}
	if err := store.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS employees (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    employee_no TEXT NOT NULL UNIQUE,
    name TEXT NOT NULL,
    department TEXT,
    enabled INTEGER NOT NULL DEFAULT 1,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS face_templates (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    employee_id INTEGER NOT NULL REFERENCES employees(id) ON DELETE CASCADE,
    embedding BLOB NOT NULL,
    embedding_dim INTEGER NOT NULL,
    image BLOB,
    image_mime_type TEXT,
    model_version TEXT NOT NULL,
    quality_score REAL,
    created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_face_templates_employee_id
    ON face_templates(employee_id);
CREATE TABLE IF NOT EXISTS users (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    username TEXT NOT NULL UNIQUE,
    display_name TEXT,
    password_hash TEXT,
    oidc_sub TEXT UNIQUE,
    role TEXT NOT NULL DEFAULT 'user',
    enabled INTEGER NOT NULL DEFAULT 1,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    token_hash TEXT NOT NULL UNIQUE,
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TEXT NOT NULL,
    last_seen_at TEXT NOT NULL,
    expires_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS api_tokens (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    token_hash TEXT NOT NULL UNIQUE,
    scopes TEXT NOT NULL,
    enabled INTEGER NOT NULL DEFAULT 1,
    created_at TEXT NOT NULL,
    last_used_at TEXT
);
CREATE INDEX IF NOT EXISTS idx_sessions_user_id ON sessions(user_id);
CREATE TABLE IF NOT EXISTS user_faces (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    embedding BLOB NOT NULL,
    embedding_dim INTEGER NOT NULL,
    image BLOB,
    image_mime_type TEXT,
    model_version TEXT NOT NULL,
    quality_score REAL,
    created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_user_faces_user_id ON user_faces(user_id);
CREATE TABLE IF NOT EXISTS login_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    method TEXT NOT NULL,
    similarity REAL NOT NULL DEFAULT 0,
    image BLOB,
    image_mime_type TEXT,
    ip TEXT,
    created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_login_events_user_id ON login_events(user_id);
CREATE TABLE IF NOT EXISTS system_settings (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS stranger_tracks (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    embedding BLOB NOT NULL,
    embedding_dim INTEGER NOT NULL,
    image BLOB,
    image_mime_type TEXT,
    quality_score REAL,
    appearance_count INTEGER NOT NULL DEFAULT 1,
    promoted_employee_id INTEGER REFERENCES employees(id),
    promoted_at TEXT,
    first_seen TEXT NOT NULL,
    last_seen TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_stranger_tracks_promoted
    ON stranger_tracks(promoted_employee_id);
`)
	if err != nil {
		return fmt.Errorf("migrating database: %w", err)
	}
	return nil
}

func utcNow() string { return time.Now().UTC().Format(time.RFC3339) }

func mapEmployeeError(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "UNIQUE constraint failed: employees.employee_no") {
		return ErrDuplicateEmployeeNo
	}
	return err
}

// CreateEmployee inserts a profile and returns the stored row.
func (s *Store) CreateEmployee(ctx context.Context, employeeNo, name, department string, enabled bool) (*Employee, error) {
	now := utcNow()
	result, err := s.db.ExecContext(ctx,
		`INSERT INTO employees (employee_no, name, department, enabled, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		employeeNo, name, department, enabled, now, now)
	if err != nil {
		return nil, mapEmployeeError(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return nil, err
	}
	return s.GetEmployee(ctx, id)
}

// EmployeeFilter narrows ListEmployees.
type EmployeeFilter struct {
	IncludeDisabled bool
	Page            int
	PageSize        int
}

// ListEmployees returns one page of profiles ordered by id, plus the total
// count matching the filter.
func (s *Store) ListEmployees(ctx context.Context, filter EmployeeFilter) ([]Employee, int, error) {
	where := ""
	if !filter.IncludeDisabled {
		where = "WHERE e.enabled = 1"
	}
	var total int
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM employees e "+where).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT e.id, e.employee_no, e.name, e.department, e.enabled, e.created_at, e.updated_at,
		       COUNT(t.id) AS sample_count
		FROM employees e
		LEFT JOIN face_templates t ON t.employee_id = e.id
		%s
		GROUP BY e.id
		ORDER BY e.id
		LIMIT ? OFFSET ?`, where), filter.PageSize, (filter.Page-1)*filter.PageSize)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	return scanEmployees(rows, total)
}

// GetEmployee returns one profile or ErrNotFound.
func (s *Store) GetEmployee(ctx context.Context, id int64) (*Employee, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT e.id, e.employee_no, e.name, e.department, e.enabled, e.created_at, e.updated_at,
		       COUNT(t.id) AS sample_count
		FROM employees e
		LEFT JOIN face_templates t ON t.employee_id = e.id
		WHERE e.id = ?
		GROUP BY e.id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	employees, _, err := scanEmployees(rows, 1)
	if err != nil {
		return nil, err
	}
	if len(employees) == 0 {
		return nil, ErrNotFound
	}
	return &employees[0], nil
}

// GetEmployeeByNo resolves a profile by its employee number (the external
// subject identifier).
func (s *Store) GetEmployeeByNo(ctx context.Context, employeeNo string) (*Employee, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT e.id, e.employee_no, e.name, e.department, e.enabled, e.created_at, e.updated_at,
		       COUNT(t.id) AS sample_count
		FROM employees e
		LEFT JOIN face_templates t ON t.employee_id = e.id
		WHERE e.employee_no = ?
		GROUP BY e.id`, employeeNo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	employees, _, err := scanEmployees(rows, 1)
	if err != nil {
		return nil, err
	}
	if len(employees) == 0 {
		return nil, ErrNotFound
	}
	return &employees[0], nil
}

func scanEmployees(rows *sql.Rows, total int) ([]Employee, int, error) {
	var employees []Employee
	for rows.Next() {
		var item Employee
		var department sql.NullString
		if err := rows.Scan(&item.ID, &item.EmployeeNo, &item.Name, &department,
			&item.Enabled, &item.CreatedAt, &item.UpdatedAt, &item.SampleCount); err != nil {
			return nil, 0, err
		}
		item.Department = department.String
		employees = append(employees, item)
	}
	return employees, total, rows.Err()
}

// EmployeeUpdate patches mutable profile fields; nil keeps the old value.
type EmployeeUpdate struct {
	EmployeeNo *string
	Name       *string
	Department *string
	// DepartmentSet distinguishes "clear the department" from "leave it".
	DepartmentSet bool
	Enabled       *bool
}

// UpdateEmployee applies the patch and returns the stored row.
func (s *Store) UpdateEmployee(ctx context.Context, id int64, update EmployeeUpdate) (*Employee, error) {
	assignments := []string{}
	args := []any{}
	if update.EmployeeNo != nil {
		assignments = append(assignments, "employee_no = ?")
		args = append(args, *update.EmployeeNo)
	}
	if update.Name != nil {
		assignments = append(assignments, "name = ?")
		args = append(args, *update.Name)
	}
	if update.DepartmentSet {
		assignments = append(assignments, "department = ?")
		if update.Department == nil || *update.Department == "" {
			args = append(args, nil)
		} else {
			args = append(args, *update.Department)
		}
	}
	if update.Enabled != nil {
		assignments = append(assignments, "enabled = ?")
		args = append(args, *update.Enabled)
	}
	if len(assignments) == 0 {
		return s.GetEmployee(ctx, id)
	}
	assignments = append(assignments, "updated_at = ?")
	args = append(args, utcNow(), id)
	result, err := s.db.ExecContext(ctx,
		"UPDATE employees SET "+strings.Join(assignments, ", ")+" WHERE id = ?", args...)
	if err != nil {
		return nil, mapEmployeeError(err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if affected == 0 {
		return nil, ErrNotFound
	}
	return s.GetEmployee(ctx, id)
}

// DeleteEmployee removes the profile; templates cascade.
func (s *Store) DeleteEmployee(ctx context.Context, id int64) error {
	result, err := s.db.ExecContext(ctx, "DELETE FROM employees WHERE id = ?", id)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// AddTemplate stores one enrollment sample for the employee.
func (s *Store) AddTemplate(ctx context.Context, employeeID int64, enrollment Enrollment) (*Template, error) {
	for _, value := range enrollment.Embedding {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return nil, ErrInvalidEmbedding
		}
	}
	if len(enrollment.Embedding) == 0 && len(enrollment.Image) == 0 {
		return nil, errors.New("face image is required")
	}
	if strings.TrimSpace(enrollment.ModelVersion) == "" {
		return nil, errors.New("model_version is required")
	}
	embedding := make([]byte, len(enrollment.Embedding)*4)
	for i, value := range enrollment.Embedding {
		bits := math.Float32bits(value)
		embedding[i*4] = byte(bits)
		embedding[i*4+1] = byte(bits >> 8)
		embedding[i*4+2] = byte(bits >> 16)
		embedding[i*4+3] = byte(bits >> 24)
	}
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO face_templates
		    (employee_id, embedding, embedding_dim, image, image_mime_type, model_version, quality_score, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		employeeID, embedding, len(enrollment.Embedding), enrollment.Image,
		enrollment.ImageMimeType, enrollment.ModelVersion, enrollment.QualityScore, utcNow())
	if err != nil {
		return nil, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return nil, err
	}
	return s.GetTemplate(ctx, id)
}

// GetTemplate returns one sample or ErrNotFound.
func (s *Store) GetTemplate(ctx context.Context, id int64) (*Template, error) {
	rows, err := s.db.QueryContext(ctx, templateSelect+" WHERE t.id = ?", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	templates, err := scanTemplates(rows)
	if err != nil {
		return nil, err
	}
	if len(templates) == 0 {
		return nil, ErrNotFound
	}
	return &templates[0], nil
}

const templateSelect = `
	SELECT t.id, t.employee_id, t.embedding, t.embedding_dim, t.image, t.image_mime_type,
	       t.model_version, t.quality_score, t.created_at
	FROM face_templates t`

// ListTemplates returns the employee's samples ordered by id.
func (s *Store) ListTemplates(ctx context.Context, employeeID int64) ([]Template, error) {
	rows, err := s.db.QueryContext(ctx, templateSelect+" WHERE t.employee_id = ? ORDER BY t.id", employeeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTemplates(rows)
}

func scanTemplates(rows *sql.Rows) ([]Template, error) {
	var templates []Template
	for rows.Next() {
		var item Template
		var embedding, image []byte
		var mimeType sql.NullString
		var quality sql.NullFloat64
		if err := rows.Scan(&item.ID, &item.EmployeeID, &embedding, &item.EmbeddingDim,
			&image, &mimeType, &item.ModelVersion, &quality, &item.CreatedAt); err != nil {
			return nil, err
		}
		item.Embedding = embedding
		item.Image = image
		item.HasImage = len(image) > 0
		item.ImageMimeType = mimeType.String
		item.QualityScore = quality.Float64
		templates = append(templates, item)
	}
	return templates, rows.Err()
}

// DeleteTemplate removes one sample of the employee.
func (s *Store) DeleteTemplate(ctx context.Context, employeeID, templateID int64) error {
	result, err := s.db.ExecContext(ctx,
		"DELETE FROM face_templates WHERE id = ? AND employee_id = ?", templateID, employeeID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// ListMatchTemplates loads every enabled employee's embeddings for the
// recognizer, skipping image-only legacy rows the caller cannot use.
func (s *Store) ListMatchTemplates(ctx context.Context) ([]MatchTemplate, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.id, t.employee_id, t.embedding, t.model_version, e.employee_no, e.name
		FROM face_templates t
		JOIN employees e ON e.id = t.employee_id
		WHERE e.enabled = 1 AND t.embedding_dim > 0
		ORDER BY t.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var templates []MatchTemplate
	for rows.Next() {
		var item MatchTemplate
		var blob []byte
		if err := rows.Scan(&item.TemplateID, &item.EmployeeID, &blob,
			&item.ModelVersion, &item.EmployeeNo, &item.Name); err != nil {
			return nil, err
		}
		item.Embedding = decodeFloat32(blob)
		templates = append(templates, item)
	}
	return templates, rows.Err()
}

func decodeFloat32(blob []byte) []float32 {
	values := make([]float32, len(blob)/4)
	for i := range values {
		bits := uint32(blob[i*4]) | uint32(blob[i*4+1])<<8 |
			uint32(blob[i*4+2])<<16 | uint32(blob[i*4+3])<<24
		values[i] = math.Float32frombits(bits)
	}
	return values
}
