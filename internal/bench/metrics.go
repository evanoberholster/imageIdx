package bench

import (
	"runtime"
	"testing"

	"github.com/evanoberholster/imageIdx/internal/storage"
)

// HeapBytes returns live heap bytes after a GC. It is the resident-state
// probe: call it before and after index construction to measure heap the
// index retains, excluding transient setup garbage.
func HeapBytes() uint64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// DiskBytes sums the sizes of files under dir matching patterns; an empty
// pattern list sums the whole directory.
func DiskBytes(dir string, patterns ...string) uint64 {
	if len(patterns) == 0 {
		patterns = []string{"*"}
	}
	return storage.DirSize(dir, patterns...)
}

// ReportIndexSize logs resident heap growth and on-disk bytes with per-item
// ratios, and reports bytes/ImageID as a benchmark metric so benchstat sees
// memory regressions alongside ns/op. Call it after the timed loop: b.Loop
// resets metrics when it first enters, which clears anything reported before
// it.
func ReportIndexSize(tb testing.TB, label string, heapBefore, heapAfter, diskBytes uint64, ids int) {
	tb.Helper()
	var resident, disk uint64
	if heapAfter > heapBefore {
		resident = heapAfter - heapBefore
	}
	disk = diskBytes
	perResident := ratioBytesPerID(resident, ids)
	perDisk := ratioBytesPerID(disk, ids)
	tb.Logf("[%s] n=%d resident=%s (%s/ImageID) disk=%s (%s/ImageID)",
		label, ids, byteSize(resident), byteSize(perResident), byteSize(disk), byteSize(perDisk))
	if b, ok := tb.(*testing.B); ok {
		b.ReportMetric(float64(perResident), "resident-B/ImageID")
		if disk != 0 {
			b.ReportMetric(float64(perDisk), "disk-B/ImageID")
			b.ReportMetric(float64(disk), "disk-B")
		}
	}
}

func ratioBytesPerID(bytes uint64, ids int) uint64 {
	if ids <= 0 {
		return bytes
	}
	return bytes / uint64(ids)
}

func byteSize(bytes uint64) string {
	const unit = 1024
	if bytes < unit {
		return itoa(bytes) + "B"
	}
	div, exp := uint64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return itoaFrac(bytes, div) + string("KMGTPE"[exp]) + "iB"
}

func itoa(v uint64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

func itoaFrac(v, div uint64) string {
	whole := v / div
	frac := (v % div) * 10 / div
	return itoa(whole) + "." + itoa(frac)
}
