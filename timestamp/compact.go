package timestamp

import "sort"

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
	id     ImageID
}

type compactRecords []compactRecord

func (r compactRecords) Len() int      { return len(r) }
func (r compactRecords) Swap(i, j int) { r[i], r[j] = r[j], r[i] }
func (r compactRecords) Less(i, j int) bool {
	if r[i].marker != r[j].marker {
		return r[i].marker < r[j].marker
	}
	return r[i].id < r[j].id
}

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
	sort.Sort(records)
	markers := make([]timemarker, len(records))
	ids := make([]ImageID, len(records))
	for index, record := range records {
		markers[index] = record.marker
		ids[index] = record.id
	}
	result, _ := newBase(markers, ids)
	return result
}
