package timeidx

import (
	"cmp"
	"slices"

	common "github.com/evanoberholster/imageIdx/common"
)

type compactionTask struct {
	block    *block
	expected *blockView
	frozen   *delta
}

type compactionResult struct {
	block     *block
	reclaimed *delta
}

type compactRecord struct {
	marker timemarker
	id     common.ValueID
}

type compactRecords []compactRecord

// compareCompactRecord orders by marker then image ID. slices.SortFunc
// inlines this closure; the old sort.Interface dispatch showed up as the
// top non-runtime cost in write-path CPU profiles.
func compareCompactRecord(a, b compactRecord) int {
	if result := cmp.Compare(a.marker, b.marker); result != 0 {
		return result
	}
	return cmp.Compare(a.id, b.id)
}

// compactorLoop folds frozen deltas into replacement bases. At most one
// task per block is ever outstanding: applyBatch refuses rotations while
// a frozen layer is pending, so the CompareAndSwap below cannot lose a
// race in practice. The failure branch stays as defense in depth —
// without it a lost swap would wedge its block in permanent backpressure,
// since the frozen layer would never clear and no spare would return.
func (i *Index) compactorLoop() {
	defer i.workers.Done()
	for task := range i.compactions {
		base := compact(task.expected.base, task.frozen)
		replacement := &blockView{
			base:       base,
			active:     task.expected.active,
			generation: task.expected.generation + 1,
		}
		if task.block.view.CompareAndSwap(task.expected, replacement) {
			i.readers.WaitForPriorReaders()
			task.frozen.reset()
			i.compactDone <- compactionResult{block: task.block, reclaimed: task.frozen}
			continue
		}
		i.compactDone <- compactionResult{block: task.block}
	}
}

func compact(base *base, frozen *delta) *base {
	addCount := int(frozen.addCount.Load())
	records := make(compactRecords, 0, base.len()+addCount)
	for index, id := range base.imageIDs {
		if !frozen.hasState(id) {
			records = append(records, compactRecord{marker: base.markers[index], id: id})
		}
	}
	for index := 0; index < addCount; index++ {
		id := frozen.addImageIDs[index]
		if frozen.latestAdd(id, uint32(index)) {
			records = append(records, compactRecord{marker: frozen.addMarkers[index], id: id})
		}
	}
	slices.SortFunc(records, compareCompactRecord)
	markers := make([]timemarker, len(records))
	ids := make([]common.ValueID, len(records))
	for index, record := range records {
		markers[index] = record.marker
		ids[index] = record.id
	}
	result, _ := newBase(markers, ids)
	return result
}
