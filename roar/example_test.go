package roar_test

import (
	"fmt"

	"github.com/evanoberholster/imageIdx/roar"
)

func ExampleFromSortedList() {
	bitmap, err := roar.FromSortedList([]uint64{1, 7, 1 << 16})
	if err != nil {
		panic(err)
	}

	fmt.Println(bitmap.Contains(7))
	fmt.Println(bitmap.Cardinality())
	// Output:
	// true
	// 3
}
