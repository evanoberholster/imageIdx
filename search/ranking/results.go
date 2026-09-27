package ranking

// Results is the final public output. ImageIDs[i] corresponds to Ranks[i];
// Ranks holds the final fused rank, not component signals.
type Results struct {
	ImageIDs []ImageID
	Ranks    []Rank
}

// Len returns the result count.
func (r *Results) Len() int {
	if r == nil {
		return 0
	}
	return len(r.ImageIDs)
}

// Validate checks parallel-slice correspondence.
func (r *Results) Validate() error {
	if r == nil {
		return ErrInvalidSignals
	}
	if len(r.ImageIDs) != len(r.Ranks) {
		return ErrLengthMismatch
	}
	return nil
}

// Sort orders results in place: higher Rank first, ties by ascending
// ImageID. It allocates nothing.
func (r *Results) Sort() {
	if r == nil {
		return
	}
	Sort(r.ImageIDs, r.Ranks)
}
