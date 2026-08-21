package timestamp

import "github.com/evanoberholster/imageIdx/internal/indexutil"

type readerEpoch struct {
	readers indexutil.ReaderEpoch
}

func (e *readerEpoch) enter() uint32 {
	return e.readers.Enter()
}

func (e *readerEpoch) exit(epoch uint32) {
	e.readers.Exit(epoch)
}

// synchronize advances the reader epoch and waits only for readers that could
// have observed objects retired before the advance. There is one compactor in
// V1, so epoch advancement is deliberately single-owner.
func (e *readerEpoch) synchronize() {
	e.readers.Synchronize()
}
