package idalloc

import (
	"fmt"
	"sync"
	"testing"

	"github.com/evanoberholster/imageIdx/common"
)

func BenchmarkNext(b *testing.B) {
	s := &memoryStore{}
	a, err := Open(s)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := a.Next(common.EntityImage); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkNextParallel(b *testing.B) {
	for _, workers := range []int{1, 8, 32} {
		b.Run(fmt.Sprintf("goroutines=%d", workers), func(b *testing.B) {
			s := &memoryStore{}
			a, err := Open(s)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			var wg sync.WaitGroup
			for w := 0; w < workers; w++ {
				wg.Add(1)
				go func(w int) {
					defer wg.Done()
					for i := w; i < b.N; i += workers {
						if _, err := a.Next(common.EntityImage); err != nil {
							b.Error(err)
							return
						}
					}
				}(w)
			}
			wg.Wait()
		})
	}
}
