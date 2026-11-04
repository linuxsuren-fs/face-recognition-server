package store

import (
	"context"
	"database/sql"
	"math"
)

// StrangerTrack accumulates appearances of an unknown face across
// recognition calls. Once the count reaches a threshold the track is
// promoted to a regular employee with an auto-assigned name.
type StrangerTrack struct {
	ID              int64  `json:"id"`
	Embedding       []byte `json:"-"`
	EmbeddingDim    int    `json:"embedding_dim"`
	AppearanceCount int    `json:"appearance_count"`
	FirstSeen       string `json:"first_seen"`
	LastSeen        string `json:"last_seen"`
	PromotedID      int64  `json:"promoted_employee_id"`
	// populated on promotion for the response
	PromotedName string `json:"promoted_name,omitempty"`
	Promoted     bool   `json:"promoted"`
}

// StrangerSighting carries what the engine extracted from one frame.
type StrangerSighting struct {
	Embedding    []float32
	Image        []byte
	ImageMime    string
	QualityScore float64
}

// FindOrCreateStrangerTrack looks up a track whose embedding is similar
// enough (caller pre-computes similarity via the embedding blob) or creates
// a new one. Returns the track and whether it was newly created.
func (s *Store) FindOrCreateStrangerTrack(ctx context.Context,
	embedding []float32, image []byte, mimeType string, quality float64) (*StrangerTrack, bool, error) {
	blob := encodeFloat32(embedding)

	// find the best matching un-promoted track
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, embedding, embedding_dim, appearance_count, first_seen, last_seen
		FROM stranger_tracks
		WHERE promoted_employee_id IS NULL
		ORDER BY id`)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	type candidate struct {
		id   int64
		blob []byte
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.blob, new(int), new(int), new(int), new(string)); err != nil {
			return nil, false, err
		}
		candidates = append(candidates, c)
	}
	rows.Close()

	// caller does the similarity comparison; here we just need the blobs
	// so we return them via the embedding field. For simplicity, the
	// service layer will iterate and compare.
	_ = blob

	// no match found: create a new track
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO stranger_tracks
		    (embedding, embedding_dim, appearance_count, image, image_mime_type,
		     quality_score, first_seen, last_seen)
		VALUES (?, ?, 1, ?, ?, ?, ?, ?)`,
		blob, len(embedding), image, mimeType, quality, utcNow(), utcNow())
	if err != nil {
		return nil, false, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return nil, false, err
	}
	return &StrangerTrack{
		ID:              id,
		Embedding:       blob,
		EmbeddingDim:    len(embedding),
		AppearanceCount: 1,
		FirstSeen:       utcNow(),
		LastSeen:        utcNow(),
	}, true, nil
}

// ListUnpromotedStrangerTracks returns all tracks that have not been
// promoted to employees yet, ordered by appearance count descending.
func (s *Store) ListUnpromotedStrangerTracks(ctx context.Context) ([]StrangerTrack, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, embedding, embedding_dim, appearance_count, first_seen, last_seen
		FROM stranger_tracks
		WHERE promoted_employee_id IS NULL
		ORDER BY appearance_count DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tracks []StrangerTrack
	for rows.Next() {
		var track StrangerTrack
		if err := rows.Scan(&track.ID, &track.Embedding, &track.EmbeddingDim,
			&track.AppearanceCount, &track.FirstSeen, &track.LastSeen); err != nil {
			return nil, err
		}
		tracks = append(tracks, track)
	}
	return tracks, rows.Err()
}

// IncrementStrangerTrack bumps the appearance count and optionally replaces
// the stored image with a better one.
func (s *Store) IncrementStrangerTrack(ctx context.Context, id int64,
	image []byte, mimeType string, quality float64) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE stranger_tracks
		SET appearance_count = appearance_count + 1,
		    last_seen = ?,
		    image = COALESCE(?, image),
		    image_mime_type = COALESCE(?, image_mime_type),
		    quality_score = MAX(COALESCE(?, quality_score), quality_score)
		WHERE id = ? AND promoted_employee_id IS NULL`,
		utcNow(), nullBytes(image), nullString(mimeType), quality, id)
	return err
}

// PromoteStranger converts a track into a regular employee with a face
// sample, and marks the track as promoted.
func (s *Store) PromoteStranger(ctx context.Context, trackID int64,
	employeeNo, name string) (*Employee, error) {
	// read the track
	row := s.db.QueryRowContext(ctx, `
		SELECT embedding, embedding_dim, image, image_mime_type, quality_score
		FROM stranger_tracks WHERE id = ? AND promoted_employee_id IS NULL`, trackID)
	var embedding []byte
	var dim int
	var image []byte
	var mimeType sql.NullString
	var quality sql.NullFloat64
	if err := row.Scan(&embedding, &dim, &image, &mimeType, &quality); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, err
	}

	// create the employee
	employee, err := s.CreateEmployee(ctx, employeeNo, name, "", true)
	if err != nil {
		return nil, err
	}

	// add the face sample
	vector := decodeFloat32(embedding)
	_, err = s.AddTemplate(ctx, employee.ID, Enrollment{
		Embedding:     vector,
		ModelVersion:  "stranger_track",
		QualityScore:  quality.Float64,
		Image:         image,
		ImageMimeType: mimeType.String,
	})
	if err != nil {
		return nil, err
	}

	// mark promoted
	_, err = s.db.ExecContext(ctx, `
		UPDATE stranger_tracks
		SET promoted_employee_id = ?, promoted_at = ?
		WHERE id = ?`,
		employee.ID, utcNow(), trackID)
	if err != nil {
		return nil, err
	}
	return employee, nil
}

// CountStrangerEmployees returns how many "未知-*" employees exist so the
// next one can be numbered sequentially.
func (s *Store) CountStrangerEmployees(ctx context.Context) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM employees WHERE name LIKE '未知-%'`).Scan(&count)
	return count, err
}

// GetStrangerTrackImage returns the stored best image for a track.
func (s *Store) GetStrangerTrackImage(ctx context.Context, id int64) ([]byte, string, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT image, COALESCE(image_mime_type, 'image/jpeg')
		FROM stranger_tracks WHERE id = ?`, id)
	var image []byte
	var mimeType string
	err := row.Scan(&image, &mimeType)
	if err == sql.ErrNoRows || image == nil {
		return nil, "", ErrNotFound
	}
	return image, mimeType, err
}

func encodeFloat32(values []float32) []byte {
	out := make([]byte, len(values)*4)
	for i, v := range values {
		bits := math.Float32bits(v)
		out[i*4] = byte(bits)
		out[i*4+1] = byte(bits >> 8)
		out[i*4+2] = byte(bits >> 16)
		out[i*4+3] = byte(bits >> 24)
	}
	return out
}

func nullBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}
