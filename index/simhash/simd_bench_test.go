package simhash

import (
	"fmt"
	"testing"
)

// BenchmarkSearchPDQBulkVsScalar compares the SIMD block kernel (quiescent
// bulk scan) with the concurrent-safe scalar scan.
func BenchmarkSearchPDQBulkVsScalar(b *testing.B) {
	st, records := benchSetup(b, 1_000_000, defaultSeqBlockRecords)
	defer st.Close()
	queries := benchQueries(records, 64, 12)
	for _, maxD := range []uint16{0, 16, 31} {
		b.Run(fmt.Sprintf("scalar/d=%d", maxD), func(b *testing.B) {
			dst := make([]PDQMatch, 0, 256)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				dst = st.SearchPDQ(dst[:0], queries[i%len(queries)], maxD)
			}
		})
		b.Run(fmt.Sprintf("bulk/d=%d", maxD), func(b *testing.B) {
			dst := make([]PDQMatch, 0, 256)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				dst = st.SearchPDQBulk(dst[:0], queries[i%len(queries)], maxD)
			}
		})
	}
}
