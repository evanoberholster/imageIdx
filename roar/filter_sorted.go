package roar

// FilterSorted appends the candidates present in b to dst. Candidates must
// arrive in non-decreasing order (the order search plans produce after
// sortUnique). A sticky cursor reuses the loaded container across runs of
// IDs that share a key, so sorted probes avoid repeating the key-table
// search that Contains pays per ID. It allocates nothing and supports
// in-place filtering (dst may alias candidates' backing store).
func (b Bitmap) FilterSorted(dst, candidates []uint64) []uint64 {
	if len(candidates) == 0 || len(b.data) == 0 {
		return dst
	}
	cursor := b.NewCursor()
	for _, id := range candidates {
		if id == InvalidImageID {
			continue
		}
		if cursor.Contains(id) {
			dst = append(dst, id)
		}
	}
	return dst
}
