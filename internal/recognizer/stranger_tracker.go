package recognizer

import (
	"context"
	"fmt"
	"math"

	"github.com/LinuxSuRen/face-recognition-server/internal/store"
)

// DefaultStrangerPromotionCount is how many distinct appearances an
// unknown face needs before it is auto-enrolled.
const DefaultStrangerPromotionCount = 3

// StrangerTracker accumulates unknown faces and promotes them to employees
// after enough appearances.
type StrangerTracker struct {
	Store *store.Store
	// KnownThreshold is the cosine similarity above which two stranger
	// sightings belong to the same person.
	KnowmThreshold float64
}

// Track processes one unknown face: finds or creates a stranger track, and
// promotes it to an employee when the appearance count reaches the
// threshold. Returns the track and the promoted employee (nil if not yet
// promoted).
func (t *StrangerTracker) Track(ctx context.Context,
	embedding []float32, image []byte, mimeType string, quality float64,
	promotionCount int) (*store.StrangerTrack, *store.Employee, error) {
	if promotionCount <= 0 {
		promotionCount = DefaultStrangerPromotionCount
	}

	// find the best matching un-promoted track
	tracks, err := t.Store.ListUnpromotedStrangerTracks(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("listing stranger tracks: %w", err)
	}
	bestScore := math.Inf(-1)
	var bestTrack *store.StrangerTrack
	for i := range tracks {
		score := cosineBlobs(embedding, tracks[i].Embedding)
		if score > bestScore {
			bestScore = score
			bestTrack = &tracks[i]
		}
	}

	if bestTrack != nil && bestScore >= t.KnowmThreshold {
		// same stranger seen again: increment
		if err := t.Store.IncrementStrangerTrack(ctx, bestTrack.ID,
			image, mimeType, quality); err != nil {
			return bestTrack, nil, err
		}
		bestTrack.AppearanceCount++
		if bestTrack.AppearanceCount >= promotionCount {
			// promote to employee
			count, err := t.Store.CountStrangerEmployees(ctx)
			if err != nil {
				return bestTrack, nil, err
			}
			name := fmt.Sprintf("未知-%d", count+1)
			employeeNo := fmt.Sprintf("STRANGER-%d", count+1)
			employee, err := t.Store.PromoteStranger(ctx, bestTrack.ID,
				employeeNo, name)
			if err != nil {
				return bestTrack, nil, err
			}
			return bestTrack, employee, nil
		}
		return bestTrack, nil, nil
	}

	// new stranger: create a track
	track, _, err := t.Store.FindOrCreateStrangerTrack(ctx,
		embedding, image, mimeType, quality)
	if err != nil {
		return nil, nil, err
	}
	return track, nil, nil
}

// cosineBlobs compares a float32 slice against a little-endian blob.
func cosineBlobs(query []float32, blob []byte) float64 {
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
