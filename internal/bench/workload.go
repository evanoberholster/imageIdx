package bench

// Op is one planned step of a mixed workload.
type Op uint8

const (
	// Read is a lookup or search operation.
	Read Op = iota
	// Write is an insert or update operation.
	Write
	// Delete is a removal operation.
	Delete
)

// String returns a stable label.
func (o Op) String() string {
	switch o {
	case Read:
		return "Read"
	case Write:
		return "Write"
	case Delete:
		return "Delete"
	default:
		return "Unknown"
	}
}

// Plan returns n deterministic operations with writePct writes and
// deletePct deletes; the remainder are reads. Percentages are clamped so
// the three counts sum to n. The plan approximates a real image-library mix
// such as 95/5, 90/10, or 80/20 read/write.
func Plan(seed int64, n, writePct, deletePct int) []Op {
	if n < 0 {
		n = 0
	}
	if writePct < 0 {
		writePct = 0
	}
	if deletePct < 0 {
		deletePct = 0
	}
	if writePct+deletePct > 100 {
		writePct = 100 - deletePct
		if writePct < 0 {
			writePct = 0
		}
	}
	out := make([]Op, n)
	for i := range out {
		out[i] = Read
	}
	if writePct == 0 && deletePct == 0 {
		return out
	}
	rng := NewRNG(seed)
	writes := n * writePct / 100
	deletes := n * deletePct / 100
	// Assign positions deterministically: mark the first writes positions,
	// then reservoir-style redistribute with a shuffled index order.
	order := rng.Perm(n)
	for _, idx := range order[:writes] {
		out[idx] = Write
	}
	for _, idx := range order[writes : writes+deletes] {
		out[idx] = Delete
	}
	return out
}

// MixLabel returns a stable benchstat label such as "95R-5W" or
// "80R-15W-5D" for a plan's percentages.
func MixLabel(writePct, deletePct int) string {
	readPct := 100 - writePct - deletePct
	label := itoa(uint64(readPct)) + "R-" + itoa(uint64(writePct)) + "W"
	if deletePct != 0 {
		label += "-" + itoa(uint64(deletePct)) + "D"
	}
	return label
}
