package auth

import (
	"context"
	"errors"
	"math"

	"github.com/LinuxSuRen/face-recognition-server/internal/engine"
	"github.com/LinuxSuRen/face-recognition-server/internal/store"
)

// Face login reasons surfaced to the UI.
var (
	// ErrNoFace means the capture carried no usable face.
	ErrNoFace = errors.New("未检测到清晰人脸")
	// ErrNoFaceBound means no account has enrolled a face yet.
	ErrNoFaceBound = errors.New("没有人脸绑定的账号")
	// ErrFaceNotMatched means the face resembles no bound sample.
	ErrFaceNotMatched = errors.New("人脸与任何账号均不匹配")
)

// FaceLogin authenticates a platform account by face: extract the largest
// face, match it against every bound sample (max per user, like the
// recognizer) and open a session above the known threshold.
func (s *Service) FaceLogin(ctx context.Context, image *engine.Image,
	eng engine.Engine, knownThreshold float64) (string, *store.User, float64, error) {
	faces, err := eng.Extract(image)
	if err != nil {
		return "", nil, 0, err
	}
	if len(faces) == 0 || faces[0].Embedding == nil {
		return "", nil, 0, ErrNoFace
	}
	bound, err := s.Store.AllUserFaces(ctx)
	if err != nil {
		return "", nil, 0, err
	}
	bestUser := int64(0)
	bestScore := math.Inf(-1)
	matched := false
	for _, face := range bound {
		if face.EmbeddingDim == 0 || face.Embedding == nil {
			continue
		}
		score := maxCosine(faces[0].Embedding, face.Embedding)
		if score > bestScore {
			bestScore = score
			bestUser = face.UserID
			matched = true
		}
	}
	if !matched {
		return "", nil, 0, ErrNoFaceBound
	}
	if bestScore < knownThreshold {
		return "", nil, bestScore, ErrFaceNotMatched
	}
	user, err := s.Store.GetUser(ctx, bestUser)
	if err != nil || !user.Enabled {
		return "", nil, bestScore, ErrFaceNotMatched
	}
	token, err := s.CreateSessionForUser(ctx, user.ID)
	if err != nil {
		return "", nil, bestScore, err
	}
	return token, user, bestScore, nil
}

// CheckFaceDuplicate reports whether the given embedding matches a face
// already bound to a different account (above the known threshold).
// Returns the conflicting username and true if found.
func (s *Service) CheckFaceDuplicate(ctx context.Context,
	embedding []float32, excludeUserID int64, threshold float64) (string, bool, error) {
	others, err := s.Store.AllOtherUserFaces(ctx, excludeUserID)
	if err != nil {
		return "", false, err
	}
	for _, other := range others {
		if score := maxCosine(embedding, other.Embedding); score >= threshold {
			return other.Username, true, nil
		}
	}
	return "", false, nil
}

// maxCosine compares two little-endian float32 blobs.
func maxCosine(query []float32, blob []byte) float64 {
	if len(blob) != len(query)*4 {
		return math.Inf(-1)
	}
	values := make([]float32, len(query))
	for i := range values {
		bits := uint32(blob[i*4]) | uint32(blob[i*4+1])<<8 |
			uint32(blob[i*4+2])<<16 | uint32(blob[i*4+3])<<24
		values[i] = math.Float32frombits(bits)
	}
	var dot, na, nb float64
	for i := range query {
		dot += float64(query[i]) * float64(values[i])
		na += float64(query[i]) * float64(query[i])
		nb += float64(values[i]) * float64(values[i])
	}
	if na <= 1e-12 || nb <= 1e-12 {
		return math.Inf(-1)
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
