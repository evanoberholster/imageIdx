package geoidx

import (
	"cmp"
	"slices"

	"github.com/golang/geo/s2"
)

type coverCandidate struct {
	cell     s2.CellID
	terminal bool
}

// CoverWorkspace owns reusable covering scratch. It is not safe for concurrent
// use. A workspace never grows during CoverRegion.
type CoverWorkspace struct {
	candidates []coverCandidate
}

// NewCoverWorkspace allocates scratch once for subsequent zero-allocation
// covers. Capacity should be at least the requested MaxCells and six.
func NewCoverWorkspace(capacity int) *CoverWorkspace {
	if capacity < 0 {
		capacity = 0
	}
	return &CoverWorkspace{candidates: make([]coverCandidate, 0, capacity)}
}

// CoverOptions controls region covering. MaxCells zero uses Config. A non-nil
// Workspace selects the no-growth, zero-allocation path.
type CoverOptions struct {
	MaxCells  int
	Workspace *CoverWorkspace
}

// CoverRegion appends sorted, normalized level-16 descendant ranges covering
// region. With a sufficiently sized Workspace and dst, the coverer allocates
// nothing; a Region implementation may allocate inside its own methods.
func (g *Index) CoverRegion(dst []Range, region s2.Region, options CoverOptions) ([]Range, error) {
	if region == nil {
		return dst, ErrInvalidCoordinate
	}
	maxCells := options.MaxCells
	if maxCells == 0 {
		maxCells = g.config.MaxCoverCells
	}
	if maxCells < 1 {
		return dst, ErrInvalidConfig
	}
	workspace := options.Workspace
	noGrow := workspace != nil
	if workspace == nil {
		workspace = NewCoverWorkspace(max(maxCells, 6))
	}
	candidates, err := coverCandidates(region, maxCells, workspace)
	if err != nil {
		return dst, err
	}
	if noGrow && cap(dst)-len(dst) < len(candidates) {
		return dst, ErrCoverBufferFull
	}
	start := len(dst)
	for _, candidate := range candidates {
		first, last := candidate.cell, candidate.cell
		if candidate.cell.Level() < GeoS2Level {
			first = candidate.cell.ChildBeginAtLevel(GeoS2Level)
			last = candidate.cell.ChildEndAtLevel(GeoS2Level).Prev()
		}
		keyRange := Range{Start: EncodeS2Key(uint64(first)), End: EncodeS2Key(uint64(last))}
		if len(dst) != start {
			previous := &dst[len(dst)-1]
			if keyRange.Start <= previous.End || previous.End <= ^S2Key(0)-2 && keyRange.Start <= previous.End+2 {
				if keyRange.End > previous.End {
					previous.End = keyRange.End
				}
				continue
			}
		}
		dst = append(dst, keyRange)
	}
	return dst, nil
}

func coverCandidates(region s2.Region, maxCells int, workspace *CoverWorkspace) ([]coverCandidate, error) {
	candidates := workspace.candidates[:0]
	appendCandidate := func(candidate coverCandidate) bool {
		if len(candidates) == cap(candidates) {
			return false
		}
		candidates = append(candidates, candidate)
		return true
	}
	for face := 0; face < 6; face++ {
		cellID := s2.CellIDFromFace(face)
		cell := s2.CellFromCellID(cellID)
		if region.IntersectsCell(cell) && !appendCandidate(coverCandidate{cell: cellID, terminal: region.ContainsCell(cell)}) {
			return nil, ErrCoverWorkspaceFull
		}
	}

	for {
		selected := -1
		selectedLevel := GeoS2Level + 1
		for index, candidate := range candidates {
			level := candidate.cell.Level()
			if !candidate.terminal && level < selectedLevel {
				selected, selectedLevel = index, level
			}
		}
		if selected < 0 {
			break
		}
		parent := candidates[selected]
		if parent.cell.Level() >= GeoS2Level {
			candidates[selected].terminal = true
			continue
		}
		var children [4]coverCandidate
		childCount := 0
		for _, childID := range parent.cell.Children() {
			child := s2.CellFromCellID(childID)
			if !region.IntersectsCell(child) {
				continue
			}
			children[childCount] = coverCandidate{
				cell: childID, terminal: childID.Level() == GeoS2Level || region.ContainsCell(child),
			}
			childCount++
		}
		if childCount == 0 {
			candidates[selected] = candidates[len(candidates)-1]
			candidates = candidates[:len(candidates)-1]
			continue
		}
		if childCount != 1 && len(candidates)+childCount-1 > maxCells {
			candidates[selected].terminal = true
			continue
		}
		if len(candidates)+childCount-1 > cap(candidates) {
			return nil, ErrCoverWorkspaceFull
		}
		candidates[selected] = children[0]
		candidates = append(candidates, children[1:childCount]...)
	}
	slices.SortFunc(candidates, func(left, right coverCandidate) int {
		return cmp.Compare(left.cell, right.cell)
	})
	workspace.candidates = candidates[:0]
	return candidates, nil
}
