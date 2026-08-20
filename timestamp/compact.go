package timestamp

import "sort"

type compactionTask struct {
	block    *TimestampBlock
	expected *TimestampBlockView
	frozen   *TimestampDelta
}

type compactionResult struct {
	block     *TimestampBlock
	reclaimed *TimestampDelta
}

type compactRecord struct {
	marker Timemarker
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
		base := compact(task.expected.Base, task.frozen)
		replacement := &TimestampBlockView{
			Base:       base,
			Active:     task.expected.Active,
			Generation: task.expected.Generation + 1,
		}
		if task.block.view.CompareAndSwap(task.expected, replacement) {
			i.epoch.synchronize()
			task.frozen.reset()
			i.compactDone <- compactionResult{block: task.block, reclaimed: task.frozen}
			continue
		}
		i.compactDone <- compactionResult{block: task.block}
	}
}

func compact(base *TimestampBase, frozen *TimestampDelta) *TimestampBase {
	addCount := int(frozen.addCount.Load())
	records := make(compactRecords, 0, base.Len()+addCount)
	for index, id := range base.ImageIDs {
		if !frozen.hasState(id) {
			records = append(records, compactRecord{marker: base.Markers[index], id: id})
		}
	}
	for index := 0; index < addCount; index++ {
		id := frozen.AddImageIDs[index]
		if frozen.latestAdd(id, uint32(index)) {
			records = append(records, compactRecord{marker: frozen.AddMarkers[index], id: id})
		}
	}
	sort.Sort(records)
	markers := make([]Timemarker, len(records))
	ids := make([]ImageID, len(records))
	for index, record := range records {
		markers[index] = record.marker
		ids[index] = record.id
	}
	result, _ := NewTimestampBase(markers, ids)
	return result
}
