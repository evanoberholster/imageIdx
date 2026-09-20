package bitmapidx

import (
	"cmp"
	"slices"

	"github.com/golang/geo/s2"
)

type geoCoverCandidate struct {
	cell     s2.CellID
	terminal bool
}

// GeoCoverWorkspace owns reusable covering scratch across CoverRegion
// calls. It is not safe for concurrent use and never grows during a
// cover: undersized workspaces fail with ErrCoverWorkspaceFull instead of
// allocating.
type GeoCoverWorkspace struct {
	candidates []geoCoverCandidate
}

// NewGeoCoverWorkspace allocates scratch for subsequent covers.
// Capacity should be at least the requested MaxCells and six (one per
// cube face); negative capacities clamp to zero.
func NewGeoCoverWorkspace(capacity int) *GeoCoverWorkspace {
	if capacity < 0 {
		capacity = 0
	}
	return &GeoCoverWorkspace{candidates: make([]geoCoverCandidate, 0, capacity)}
}

// GeoCoverOptions controls region covering. MaxCells caps emitted cells
// (zero selects DefaultGeoCoverCells); Workspace, when non-nil, supplies
// bounded scratch and requires dst capacity for the result. MaxCells is a
// soft cap: covering starts from the six cube faces, so a region that
// intersects every face emits at least six cells regardless of a smaller
// budget (see CoverRegion).
type GeoCoverOptions struct {
	MaxCells  int
	Workspace *GeoCoverWorkspace
}

// CoverRegion appends sorted, normalized level-16 descendant ranges
// covering region, preserving dst's prefix. With a caller workspace and
// sufficient dst capacity it allocates nothing itself; the Region
// implementation may allocate inside IntersectsCell/ContainsCell. A
// caller workspace with insufficient dst capacity fails with
// ErrCoverBufferFull instead of growing dst. MaxCells is not a strict
// lower bound: covering starts from the six cube faces, so a region that
// intersects all faces yields at least six ranges even when MaxCells is
// smaller.
func CoverRegion(dst []GeoRange, region s2.Region, options GeoCoverOptions) ([]GeoRange, error) {
	if region == nil {
		return dst, ErrInvalidRegion
	}
	maxCells := options.MaxCells
	if maxCells == 0 {
		maxCells = DefaultGeoCoverCells
	}
	if maxCells < 1 {
		return dst, ErrInvalidConfig
	}
	workspace := options.Workspace
	noGrow := workspace != nil
	if workspace == nil {
		workspace = NewGeoCoverWorkspace(max(maxCells, 6))
	}
	candidates, err := geoCoverCandidates(region, maxCells, workspace)
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
		keyRange := GeoRange{Start: uint64(first) >> geoKeyShift, End: uint64(last) >> geoKeyShift}
		if len(dst) != start {
			previous := &dst[len(dst)-1]
			if geoRangesMerge(previous.End, keyRange.Start) {
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

// geoCoverCandidates refines the six cube faces into at most maxCells
// terminal cells covering region, using only workspace capacity. The
// returned slice aliases workspace backing and stays valid only until the
// next cover on the same workspace; the workspace length resets to zero
// on return, so callers must consume candidates (as CoverRegion does)
// before reusing the workspace.
func geoCoverCandidates(region s2.Region, maxCells int, workspace *GeoCoverWorkspace) ([]geoCoverCandidate, error) {
	candidates := workspace.candidates[:0]
	appendCandidate := func(candidate geoCoverCandidate) bool {
		if len(candidates) == cap(candidates) {
			return false
		}
		candidates = append(candidates, candidate)
		return true
	}
	for face := 0; face < 6; face++ {
		cellID := s2.CellIDFromFace(face)
		cell := s2.CellFromCellID(cellID)
		if region.IntersectsCell(cell) && !appendCandidate(geoCoverCandidate{cell: cellID, terminal: region.ContainsCell(cell)}) {
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
		var children [4]geoCoverCandidate
		childCount := 0
		for _, childID := range parent.cell.Children() {
			child := s2.CellFromCellID(childID)
			if !region.IntersectsCell(child) {
				continue
			}
			children[childCount] = geoCoverCandidate{
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
	slices.SortFunc(candidates, func(left, right geoCoverCandidate) int {
		return cmp.Compare(left.cell, right.cell)
	})
	workspace.candidates = candidates[:0]
	return candidates, nil
}
