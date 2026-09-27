package ranking

// worse reports whether a should sort after b: smaller rank first, then
// larger ImageID. Higher rank is better, so the worst is the smallest rank.
func worse(rankA Rank, idA ImageID, rankB Rank, idB ImageID) bool {
	if rankA != rankB {
		return rankA < rankB
	}
	return idA > idB
}

// better reports whether a should sort before b.
func better(rankA Rank, idA ImageID, rankB Rank, idB ImageID) bool {
	if rankA != rankB {
		return rankA > rankB
	}
	return idA < idB
}

func swapPair(ids []ImageID, ranks []Rank, i, j int) {
	ids[i], ids[j] = ids[j], ids[i]
	ranks[i], ranks[j] = ranks[j], ranks[i]
}

func insertionRange(ids []ImageID, ranks []Rank, lo, hi int) {
	for i := lo + 1; i <= hi; i++ {
		for j := i; j > lo && better(ranks[j], ids[j], ranks[j-1], ids[j-1]); j-- {
			swapPair(ids, ranks, j, j-1)
		}
	}
}

func sortRange(ids []ImageID, ranks []Rank, lo, hi int) {
	for hi-lo > 16 {
		mid := lo + (hi-lo)/2
		if better(ranks[mid], ids[mid], ranks[lo], ids[lo]) {
			swapPair(ids, ranks, lo, mid)
		}
		if better(ranks[hi], ids[hi], ranks[lo], ids[lo]) {
			swapPair(ids, ranks, lo, hi)
		}
		if better(ranks[hi], ids[hi], ranks[mid], ids[mid]) {
			swapPair(ids, ranks, mid, hi)
		}
		// Median is now at mid; move to hi as Lomuto pivot.
		swapPair(ids, ranks, mid, hi)
		pivotRank, pivotID := ranks[hi], ids[hi]
		i := lo
		for j := lo; j < hi; j++ {
			if better(ranks[j], ids[j], pivotRank, pivotID) {
				swapPair(ids, ranks, i, j)
				i++
			}
		}
		swapPair(ids, ranks, i, hi)
		// Recurse on the smaller side, loop on the larger to bound depth.
		if i-lo < hi-i {
			sortRange(ids, ranks, lo, i-1)
			lo = i + 1
		} else {
			sortRange(ids, ranks, i+1, hi)
			hi = i - 1
		}
	}
	insertionRange(ids, ranks, lo, hi)
}

// Sort orders parallel slices in place: higher Rank first, ties by ascending
// ImageID. It allocates nothing and assumes len(ids)==len(ranks); mismatched
// inputs are left unchanged.
func Sort(ids []ImageID, ranks []Rank) {
	if len(ids) != len(ranks) || len(ids) < 2 {
		return
	}
	sortRange(ids, ranks, 0, len(ids)-1)
}

func siftWorstUp(ids []ImageID, ranks []Rank, index int) {
	for index > 0 {
		parent := (index - 1) / 2
		if !worse(ranks[index], ids[index], ranks[parent], ids[parent]) {
			return
		}
		ids[index], ids[parent] = ids[parent], ids[index]
		ranks[index], ranks[parent] = ranks[parent], ranks[index]
		index = parent
	}
}

func siftWorstDown(ids []ImageID, ranks []Rank, index int) {
	n := len(ids)
	for {
		left, right := 2*index+1, 2*index+2
		worst := index
		if left < n && worse(ranks[left], ids[left], ranks[worst], ids[worst]) {
			worst = left
		}
		if right < n && worse(ranks[right], ids[right], ranks[worst], ids[worst]) {
			worst = right
		}
		if worst == index {
			return
		}
		ids[index], ids[worst] = ids[worst], ids[index]
		ranks[index], ranks[worst] = ranks[worst], ranks[index]
		index = worst
	}
}

// TopK copies the best k candidates from src into dst and sorts them
// descending (higher Rank first, ImageID tie-break). Positional
// correspondence is preserved; src stays unsorted.
//
//   - k < 0 selects all candidates.
//   - k == 0 returns dst unchanged.
//   - k >= n copies and sorts all.
//
// dstIDs/dstRanks are caller-owned and must have capacity for the selected
// count; growth may allocate once, reuse is allocation-free. srcIDs/srcRanks
// must be parallel. Mismatched src returns dst unchanged.
func TopK(dstIDs []ImageID, dstRanks []Rank, srcIDs []ImageID, srcRanks []Rank, k int) ([]ImageID, []Rank) {
	if len(srcIDs) != len(srcRanks) {
		return dstIDs, dstRanks
	}
	n := len(srcIDs)
	if k == 0 || n == 0 {
		return dstIDs, dstRanks
	}
	if k < 0 || k >= n {
		dstIDs = append(dstIDs[:0], srcIDs...)
		dstRanks = append(dstRanks[:0], srcRanks...)
		Sort(dstIDs, dstRanks)
		return dstIDs, dstRanks
	}
	// Bounded worst-heap of size k on the dst buffers.
	dstIDs = append(dstIDs[:0], srcIDs[:k]...)
	dstRanks = append(dstRanks[:0], srcRanks[:k]...)
	for i := k/2 - 1; i >= 0; i-- {
		siftWorstDown(dstIDs, dstRanks, i)
	}
	for i := k; i < n; i++ {
		if worse(dstRanks[0], dstIDs[0], srcRanks[i], srcIDs[i]) {
			dstIDs[0], dstRanks[0] = srcIDs[i], srcRanks[i]
			siftWorstDown(dstIDs, dstRanks, 0)
		} else if dstRanks[0] == srcRanks[i] && dstIDs[0] == srcIDs[i] {
			// Equal worst stays; deterministic, no swap needed.
		}
		_ = i
	}
	Sort(dstIDs, dstRanks)
	return dstIDs, dstRanks
}
