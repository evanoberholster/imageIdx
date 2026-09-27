package ranking

import "errors"

// MaxSignals bounds the active SoA lanes per query.
const MaxSignals = 4

// Signals is the intermediate SoA representation. ImageIDs[i] corresponds to
// Values[s][i] for every active lane s < Count. Lanes are execution lanes,
// not fixed meanings: one query may use {time, geo, vector, text} while
// another uses {vector, intrinsic}.
type Signals struct {
	ImageIDs []ImageID
	Values   [MaxSignals][]Rank
	Kinds    [MaxSignals]Kind
	Count    uint8
}

var (
	// ErrInvalidSignals reports structural problems: too many lanes or
	// lanes/kinds used beyond Count.
	ErrInvalidSignals = errors.New("ranking: invalid signals")
	// ErrLengthMismatch reports a lane whose length differs from ImageIDs.
	ErrLengthMismatch = errors.New("ranking: signal length mismatch")
	// ErrShortBuffer reports a caller buffer too small for the operation.
	ErrShortBuffer = errors.New("ranking: short buffer")
)

// Len returns the candidate count.
func (s *Signals) Len() int {
	if s == nil {
		return 0
	}
	return len(s.ImageIDs)
}

// Validate checks Count and positional correspondence.
func (s *Signals) Validate() error {
	if s == nil {
		return ErrInvalidSignals
	}
	if s.Count > MaxSignals {
		return ErrInvalidSignals
	}
	n := len(s.ImageIDs)
	for i := 0; i < int(s.Count); i++ {
		if len(s.Values[i]) != n {
			return ErrLengthMismatch
		}
	}
	return nil
}

// RequiredRanks returns the number of Rank slots backing count lanes of n
// candidates in the contiguous layout [N lane0][N lane1]...
func RequiredRanks(n, count int) int { return n * count }

// SplitRanks partitions buf into count lanes of n ranks each using the
// contiguous layout. It performs no allocation; buf must hold at least
// n*count slots.
func SplitRanks(buf []Rank, n, count int) ([MaxSignals][]Rank, error) {
	var out [MaxSignals][]Rank
	if count < 0 || count > MaxSignals || n < 0 {
		return out, ErrInvalidSignals
	}
	if len(buf) < n*count {
		return out, ErrShortBuffer
	}
	for i := 0; i < count; i++ {
		out[i] = buf[i*n : (i+1)*n : (i+1)*n]
	}
	return out, nil
}

// AttachContiguous binds s.Values lanes to buf, which must hold at least
// len(ImageIDs)*Count slots. Slicing only; no allocation.
func (s *Signals) AttachContiguous(buf []Rank) error {
	if s == nil {
		return ErrInvalidSignals
	}
	lanes, err := SplitRanks(buf, len(s.ImageIDs), int(s.Count))
	if err != nil {
		return err
	}
	s.Values = lanes
	return nil
}
