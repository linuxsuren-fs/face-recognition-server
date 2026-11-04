package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// User is one platform account (local password and/or federated OIDC).
type User struct {
	ID          int64  `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"` // admin | user
	Enabled     bool   `json:"enabled"`
	OidcLinked  bool   `json:"oidc_linked"`
	PasswordSet bool   `json:"password_set"`
	FaceCount   int    `json:"face_count"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

// UserUpdate patches a user; nil fields keep their values.
type UserUpdate struct {
	DisplayName *string
	Password    *string // bcrypt hash
	Role        *string
	Enabled     *bool
}

// Session is one browser login session.
type Session struct {
	TokenHash  string
	UserID     int64
	ExpiresAt  time.Time
	LastSeenAt time.Time
}

// APIToken is one external-system token (secrets never stored in clear).
type APIToken struct {
	ID         int64   `json:"id"`
	Name       string  `json:"name"`
	Scopes     string  `json:"scopes"`
	Enabled    bool    `json:"enabled"`
	CreatedAt  string  `json:"created_at"`
	LastUsedAt *string `json:"last_used_at"`
}

// CountUsers returns the number of accounts.
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM users").Scan(&count)
	return count, err
}

// CreateUser inserts an account; passwordHash may be empty for OIDC-only.
func (s *Store) CreateUser(ctx context.Context, username, displayName, passwordHash, role string, enabled bool) (*User, error) {
	now := utcNow()
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO users (username, display_name, password_hash, role, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		username, nullString(displayName), nullString(passwordHash), role, enabled, now, now)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed: users.username") {
			return nil, ErrDuplicateUsername
		}
		return nil, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return nil, err
	}
	return s.GetUser(ctx, id)
}

// ListUsers returns one page of accounts ordered by id.
func (s *Store) ListUsers(ctx context.Context, page, pageSize int) ([]User, int, error) {
	var total int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM users").Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT u.id, u.username, u.display_name, u.role, u.enabled,
		       u.password_hash IS NOT NULL AND u.password_hash != '',
		       u.oidc_sub IS NOT NULL, u.created_at, u.updated_at,
		       (SELECT COUNT(*) FROM user_faces f WHERE f.user_id = u.id)
		FROM users u ORDER BY u.id LIMIT ? OFFSET ?`,
		pageSize, (page-1)*pageSize)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	users := []User{}
	for rows.Next() {
		var user User
		var displayName sql.NullString
		if err := rows.Scan(&user.ID, &user.Username, &displayName, &user.Role, &user.Enabled,
			&user.PasswordSet, &user.OidcLinked, &user.CreatedAt, &user.UpdatedAt,
			&user.FaceCount); err != nil {
			return nil, 0, err
		}
		user.DisplayName = displayName.String
		users = append(users, user)
	}
	return users, total, rows.Err()
}

// GetUser returns one account or ErrNotFound.
func (s *Store) GetUser(ctx context.Context, id int64) (*User, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, username, display_name, role, enabled,
		       password_hash IS NOT NULL AND password_hash != '',
		       oidc_sub IS NOT NULL, created_at, updated_at
		FROM users WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	users, err := scanUsers(rows)
	if err != nil || len(users) == 0 {
		return nil, ErrNotFound
	}
	return &users[0], nil
}

// GetUserForLogin loads the credential columns for username verification.
type UserCredentials struct {
	ID           int64
	Username     string
	PasswordHash string
	Role         string
	Enabled      bool
}

func (s *Store) GetUserForLogin(ctx context.Context, username string) (*UserCredentials, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, username, COALESCE(password_hash, ''), role, enabled
		FROM users WHERE username = ?`, username)
	var creds UserCredentials
	err := row.Scan(&creds.ID, &creds.Username, &creds.PasswordHash, &creds.Role, &creds.Enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &creds, nil
}

// UpdateUser applies the patch.
func (s *Store) UpdateUser(ctx context.Context, id int64, update UserUpdate) (*User, error) {
	assignments := []string{}
	args := []any{}
	if update.DisplayName != nil {
		assignments = append(assignments, "display_name = ?")
		args = append(args, nullString(*update.DisplayName))
	}
	if update.Password != nil {
		assignments = append(assignments, "password_hash = ?")
		args = append(args, nullString(*update.Password))
	}
	if update.Role != nil {
		assignments = append(assignments, "role = ?")
		args = append(args, *update.Role)
	}
	if update.Enabled != nil {
		assignments = append(assignments, "enabled = ?")
		args = append(args, *update.Enabled)
	}
	if len(assignments) == 0 {
		return s.GetUser(ctx, id)
	}
	assignments = append(assignments, "updated_at = ?")
	args = append(args, utcNow(), id)
	result, err := s.db.ExecContext(ctx,
		"UPDATE users SET "+strings.Join(assignments, ", ")+" WHERE id = ?", args...)
	if err != nil {
		return nil, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if affected == 0 {
		return nil, ErrNotFound
	}
	return s.GetUser(ctx, id)
}

// LinkOIDC binds an external identity to a user (upsert by subject).
func (s *Store) LinkOIDC(ctx context.Context, userID int64, subject string) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE users SET oidc_sub = ?, updated_at = ? WHERE id = ?", subject, utcNow(), userID)
	return err
}

// GetUserByOIDCSubject finds the account bound to an external identity.
func (s *Store) GetUserByOIDCSubject(ctx context.Context, subject string) (*User, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, username, display_name, role, enabled,
		       password_hash IS NOT NULL AND password_hash != '',
		       oidc_sub IS NOT NULL, created_at, updated_at
		FROM users WHERE oidc_sub = ?`, subject)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	users, err := scanUsers(rows)
	if err != nil || len(users) == 0 {
		return nil, ErrNotFound
	}
	return &users[0], nil
}

func scanUsers(rows *sql.Rows) ([]User, error) {
	users := []User{}
	for rows.Next() {
		var user User
		var displayName sql.NullString
		if err := rows.Scan(&user.ID, &user.Username, &displayName, &user.Role, &user.Enabled,
			&user.PasswordSet, &user.OidcLinked, &user.CreatedAt, &user.UpdatedAt); err != nil {
			return nil, err
		}
		user.DisplayName = displayName.String
		users = append(users, user)
	}
	return users, rows.Err()
}

// ---- sessions ----

// CreateSession stores a hashed session token with the given lifetime.
func (s *Store) CreateSession(ctx context.Context, tokenHash string, userID int64, lifetime time.Duration) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions (token_hash, user_id, created_at, last_seen_at, expires_at)
		VALUES (?, ?, ?, ?, ?)`,
		tokenHash, userID, now.Format(time.RFC3339), now.Format(time.RFC3339),
		now.Add(lifetime).Format(time.RFC3339))
	return err
}

// GetSession returns the session for a token hash when still valid.
func (s *Store) GetSession(ctx context.Context, tokenHash string) (*Session, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT token_hash, user_id, last_seen_at, expires_at
		FROM sessions WHERE token_hash = ?`, tokenHash)
	var session Session
	var lastSeen, expires string
	err := row.Scan(&session.TokenHash, &session.UserID, &lastSeen, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if session.LastSeenAt, err = time.Parse(time.RFC3339, lastSeen); err != nil {
		return nil, err
	}
	if session.ExpiresAt, err = time.Parse(time.RFC3339, expires); err != nil {
		return nil, err
	}
	if time.Now().UTC().After(session.ExpiresAt) {
		_ = s.DeleteSession(ctx, tokenHash)
		return nil, ErrNotFound
	}
	return &session, nil
}

// TouchSession refreshes the last-seen timestamp (cheap, throttled by caller).
func (s *Store) TouchSession(ctx context.Context, tokenHash string) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE sessions SET last_seen_at = ? WHERE token_hash = ?",
		time.Now().UTC().Format(time.RFC3339), tokenHash)
	return err
}

// DeleteSession removes one session (logout).
func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE token_hash = ?", tokenHash)
	return err
}

// ---- api tokens ----

// CreateAPIToken stores a hashed external token.
func (s *Store) CreateAPIToken(ctx context.Context, name, tokenHash, scopes string) (*APIToken, error) {
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO api_tokens (name, token_hash, scopes, enabled, created_at)
		VALUES (?, ?, ?, 1, ?)`, name, tokenHash, scopes, utcNow())
	if err != nil {
		return nil, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return nil, err
	}
	return s.GetAPIToken(ctx, id)
}

// GetAPIToken returns metadata by id.
func (s *Store) GetAPIToken(ctx context.Context, id int64) (*APIToken, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, scopes, enabled, created_at, last_used_at
		FROM api_tokens WHERE id = ?`, id)
	return scanAPIToken(row)
}

// ListAPITokens returns all tokens ordered by id (small table).
func (s *Store) ListAPITokens(ctx context.Context) ([]APIToken, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, scopes, enabled, created_at, last_used_at
		FROM api_tokens ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tokens := []APIToken{}
	for rows.Next() {
		var token APIToken
		var lastUsed sql.NullString
		if err := rows.Scan(&token.ID, &token.Name, &token.Scopes, &token.Enabled,
			&token.CreatedAt, &lastUsed); err != nil {
			return nil, err
		}
		if lastUsed.Valid {
			value := lastUsed.String
			token.LastUsedAt = &value
		}
		tokens = append(tokens, token)
	}
	return tokens, rows.Err()
}

// LookupAPIToken resolves a hashed bearer token to its metadata.
func (s *Store) LookupAPIToken(ctx context.Context, tokenHash string) (*APIToken, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, scopes, enabled, created_at, last_used_at
		FROM api_tokens WHERE token_hash = ?`, tokenHash)
	token, err := scanAPIToken(row)
	if err != nil {
		return nil, err
	}
	if !token.Enabled {
		return nil, ErrNotFound
	}
	return token, nil
}

// TouchAPIToken records usage (throttled by the caller).
func (s *Store) TouchAPIToken(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE api_tokens SET last_used_at = ? WHERE id = ?", utcNow(), id)
	return err
}

// DeleteAPIToken revokes a token.
func (s *Store) DeleteAPIToken(ctx context.Context, id int64) error {
	result, err := s.db.ExecContext(ctx, "DELETE FROM api_tokens WHERE id = ?", id)
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

func scanAPIToken(row *sql.Row) (*APIToken, error) {
	var token APIToken
	var lastUsed sql.NullString
	err := row.Scan(&token.ID, &token.Name, &token.Scopes, &token.Enabled,
		&token.CreatedAt, &lastUsed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scanning api token: %w", err)
	}
	if lastUsed.Valid {
		value := lastUsed.String
		token.LastUsedAt = &value
	}
	return &token, nil
}

func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
