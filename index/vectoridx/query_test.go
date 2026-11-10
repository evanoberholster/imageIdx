package vectoridx

import (
	"errors"
	"math/rand"
	"testing"
)

func TestQueryElementSelection(t *testing.T) {
	cases := []struct {
		name string
		q    Query
		want ElementType
		ok   bool
	}{
		{"int8", Query{Int8: []int8{1}}, ElementInt8, true},
		{"uint8", Query{Uint8: []uint8{1}}, ElementUint8, true},
		{"int16", Query{Int16: []int16{1}}, ElementInt16, true},
		{"uint16", Query{Uint16: []uint16{1}}, ElementUint16, true},
		{"float16", Query{Float16: []uint16{1}}, ElementFloat16, true},
		{"float32", Query{Float32: []float32{1}}, ElementFloat32, true},
		{"empty", Query{}, 0, false},
		{"multiple", Query{Int8: []int8{1}, Float32: []float32{1}}, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := c.q.Element()
			if got != c.want || ok != c.ok {
				t.Fatalf("Element() = (%v, %v), want (%v, %v)", got, ok, c.want, c.ok)
			}
		})
	}
}

func TestQuerySearchMatchesTyped(t *testing.T) {
	rng := rand.New(rand.NewSource(811))
	const n = 20

	t.Run("float32", func(t *testing.T) {
		idx, err := NewMemory(Config{Namespace: testNamespace, Dimensions: Dim512, ElementType: ElementFloat32})
		if err != nil {
			t.Fatal(err)
		}
		defer idx.Close()
		vecs := make([][]float32, n)
		for i := range vecs {
			vecs[i] = randFloatVec(rng, Dim512)
			if err := idx.AddFloat32(NodeID(i+1), vecs[i]); err != nil {
				t.Fatal(err)
			}
		}
		typed, err := idx.SearchFloat32(vecs[0], nil, 5)
		if err != nil {
			t.Fatal(err)
		}
		generic, err := idx.Search(Query{Float32: vecs[0]}, SearchOptions{K: 5})
		if err != nil {
			t.Fatal(err)
		}
		assertSameResults(t, generic, typed)
		snap := idx.Snapshot()
		defer snap.Release()
		fromSnap, err := snap.Search(Query{Float32: vecs[0]}, SearchOptions{K: 5})
		if err != nil {
			t.Fatal(err)
		}
		assertSameResults(t, fromSnap, typed)
	})

	t.Run("int8", func(t *testing.T) {
		idx, err := NewMemory(Config{Namespace: testNamespace, Dimensions: Dim512, ElementType: ElementInt8})
		if err != nil {
			t.Fatal(err)
		}
		defer idx.Close()
		vecs := make([][]int8, n)
		for i := range vecs {
			vecs[i] = randInt8Vec(rng, Dim512)
			if err := idx.AddInt8(NodeID(i+1), vecs[i]); err != nil {
				t.Fatal(err)
			}
		}
		typed, err := idx.SearchInt8(vecs[0], nil, 5)
		if err != nil {
			t.Fatal(err)
		}
		generic, err := idx.Search(Query{Int8: vecs[0]}, SearchOptions{K: 5})
		if err != nil {
			t.Fatal(err)
		}
		assertSameResults(t, generic, typed)
	})

	t.Run("float16", func(t *testing.T) {
		idx, err := NewMemory(Config{Namespace: testNamespace, Dimensions: Dim512, ElementType: ElementFloat16})
		if err != nil {
			t.Fatal(err)
		}
		defer idx.Close()
		vecs := make([][]uint16, n)
		for i := range vecs {
			vecs[i] = make([]uint16, Dim512)
			for j := range vecs[i] {
				vecs[i][j] = Float32ToFloat16(rng.Float32()*4 - 2)
			}
			if err := idx.AddFloat16(NodeID(i+1), vecs[i]); err != nil {
				t.Fatal(err)
			}
		}
		typed, err := idx.SearchFloat16(vecs[0], nil, 5)
		if err != nil {
			t.Fatal(err)
		}
		generic, err := idx.Search(Query{Float16: vecs[0]}, SearchOptions{K: 5})
		if err != nil {
			t.Fatal(err)
		}
		assertSameResults(t, generic, typed)
	})
}

func TestQueryValidation(t *testing.T) {
	idx, err := NewMemory(Config{Namespace: testNamespace, Dimensions: Dim512, ElementType: ElementFloat32})
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	vec := randFloatVec(rand.New(rand.NewSource(812)), Dim512)
	for _, q := range []Query{
		{},
		{Int8: make([]int8, Dim512)},
		{Float32: vec[:511]},
		{Float32: vec, Int8: make([]int8, Dim512)},
	} {
		if _, err := idx.Search(q, SearchOptions{K: 5}); !errors.Is(err, ErrInvalidVector) {
			t.Fatalf("Search(%+v) err = %v, want ErrInvalidVector", q, err)
		}
	}
}

func assertSameResults(t *testing.T, got, want []Result) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("result count = %d, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i].ValueID != want[i].ValueID || got[i].Score != want[i].Score || got[i].Distance != want[i].Distance {
			t.Fatalf("result %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}
