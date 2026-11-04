package store

import (
	"context"
	"database/sql"
	"errors"
	"math"
)

// UserFace is one face sample bound to a platform account.
type UserFace struct {
	ID            int64   `json:"id"`
	UserID        int64   `json:"user_id"`
	EmbeddingDim  int     `json:"embedding_dim"`
	HasImage      bool    `json:"has_image"`
	ImageMimeType string  `json:"image_mime_type"`
	ModelVersion  string  `json:"model_version"`
	QualityScore  float64 `json:"quality_score"`
	CreatedAt     string  `json:"created_at"`

	// Embedding is the little-endian float32 blob; never serialized.
	Embedding []byte `json:"-"`
	// Username is only set by AllUserFaces (login matching); internal.
	Username string `json:"-"`
}

// UserFaceEnrollment carries what the engine produced for one sample.
type UserFaceEnrollment struct {
	Embedding     []float32
	ModelVersion  string
	QualityScore  float64
	Image         []byte
	ImageMimeType string
}

// AddUserFace stores one face sample for the account.
func (s *Store) AddUserFace(ctx context.Context, userID int64, enrollment UserFaceEnrollment) (*UserFace, error) {
	for _, value := range enrollment.Embedding {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return nil, ErrInvalidEmbedding
		}
	}
	if len(enrollment.Embedding) == 0 || enrollment.Image == nil {
		return nil, errors.New("face image is required")
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
		INSERT INTO user_faces
		    (user_id, embedding, embedding_dim, image, image_mime_type, model_version, quality_score, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		userID, embedding, len(enrollment.Embedding), enrollment.Image,
		enrollment.ImageMimeType, enrollment.ModelVersion,
		enrollment.QualityScore, utcNow())
	if err != nil {
		return nil, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return nil, err
	}
	return s.GetUserFace(ctx, id)
}

const userFaceSelect = `
	SELECT f.id, f.user_id, f.embedding, f.embedding_dim, f.image, f.image_mime_type,
	       f.model_version, f.quality_score, f.created_at
	FROM user_faces f`

// GetUserFace returns one sample.
func (s *Store) GetUserFace(ctx context.Context, id int64) (*UserFace, error) {
	row := s.db.QueryRowContext(ctx, userFaceSelect+" WHERE f.id = ?", id)
	face, err := scanUserFace(row)
	if err != nil {
		return nil, err
	}
	return face, nil
}

// ListUserFaces returns the account's samples ordered by id.
func (s *Store) ListUserFaces(ctx context.Context, userID int64) ([]UserFace, error) {
	rows, err := s.db.QueryContext(ctx,
		userFaceSelect+" WHERE f.user_id = ? ORDER BY f.id", userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	faces, err := scanUserFaces(rows)
	if err != nil {
		return nil, err
	}
	for i := range faces {
		faces[i].Embedding = nil // listing never carries vectors
	}
	return faces, nil
}

// AllUserFaces loads every sample of enabled accounts for face login.
func (s *Store) AllUserFaces(ctx context.Context) ([]UserFace, error) {
	return s.allUserFaces(ctx, 0)
}

// AllOtherUserFaces loads every sample of enabled accounts except the given
// user — used to reject binding a face that already belongs to someone else.
func (s *Store) AllOtherUserFaces(ctx context.Context, excludeUserID int64) ([]UserFace, error) {
	return s.allUserFaces(ctx, excludeUserID)
}

func (s *Store) allUserFaces(ctx context.Context, excludeUserID int64) ([]UserFace, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT f.id, f.user_id, f.embedding, f.embedding_dim, f.image, f.image_mime_type,
		       f.model_version, f.quality_score, f.created_at, u.username
		FROM user_faces f
		JOIN users u ON u.id = f.user_id
		WHERE u.enabled = 1 AND (? = 0 OR f.user_id != ?)
		ORDER BY f.id`, excludeUserID, excludeUserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var faces []UserFace
	for rows.Next() {
		var face UserFace
		var embedding, image []byte
		var mimeType sql.NullString
		var quality sql.NullFloat64
		if err := rows.Scan(&face.ID, &face.UserID, &embedding, &face.EmbeddingDim,
			&image, &mimeType, &face.ModelVersion, &quality, &face.CreatedAt,
			&face.Username); err != nil {
			return nil, err
		}
		face.Embedding = embedding
		face.HasImage = len(image) > 0
		face.ImageMimeType = mimeType.String
		face.QualityScore = quality.Float64
		faces = append(faces, face)
	}
	return faces, rows.Err()
}

// DeleteUserFace removes one sample of the account.
func (s *Store) DeleteUserFace(ctx context.Context, userID, id int64) error {
	result, err := s.db.ExecContext(ctx,
		"DELETE FROM user_faces WHERE id = ? AND user_id = ?", id, userID)
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

func scanUserFace(row *sql.Row) (*UserFace, error) {
	var face UserFace
	var embedding, image []byte
	var mimeType sql.NullString
	var quality sql.NullFloat64
	err := row.Scan(&face.ID, &face.UserID, &embedding, &face.EmbeddingDim,
		&image, &mimeType, &face.ModelVersion, &quality, &face.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	face.Embedding = embedding
	face.HasImage = len(image) > 0
	face.ImageMimeType = mimeType.String
	face.QualityScore = quality.Float64
	return &face, nil
}

func scanUserFaces(rows *sql.Rows) ([]UserFace, error) {
	var faces []UserFace
	for rows.Next() {
		var face UserFace
		var embedding, image []byte
		var mimeType sql.NullString
		var quality sql.NullFloat64
		if err := rows.Scan(&face.ID, &face.UserID, &embedding, &face.EmbeddingDim,
			&image, &mimeType, &face.ModelVersion, &quality, &face.CreatedAt); err != nil {
			return nil, err
		}
		face.HasImage = len(image) > 0
		face.ImageMimeType = mimeType.String
		face.QualityScore = quality.Float64
		faces = append(faces, face)
	}
	return faces, rows.Err()
}
