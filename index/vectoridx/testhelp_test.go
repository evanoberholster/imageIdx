package vectoridx

import (
	"math/rand"
	"testing"

	schema "github.com/evanoberholster/imageIdx/schema"
)

const testNamespace schema.Namespace = 1

func testConfigFloat512() Config {
	return Config{Namespace: testNamespace, Dimensions: Dim512, ElementType: ElementFloat32}
}

func randFloatVec(rng *rand.Rand, dims Dimensions) []float32 {
	v := make([]float32, dims)
	for i := range v {
		v[i] = rng.Float32()*4 - 2
	}
	return v
}

func randInt8Vec(rng *rand.Rand, dims Dimensions) []int8 {
	v := make([]int8, dims)
	for i := range v {
		v[i] = int8(rng.Intn(256) - 128)
	}
	return v
}

func mustAddFloat(t *testing.T, idx *VectorIdx, id NodeID, vec []float32) {
	t.Helper()
	if err := idx.AddFloat32(id, vec); err != nil {
		t.Fatalf("AddFloat32(%d): %v", id, err)
	}
}

func resultIDs(results []Result) []schema.ValueID {
	ids := make([]schema.ValueID, len(results))
	for i, r := range results {
		ids[i] = r.ValueID
	}
	return ids
}

func containsResult(results []Result, id schema.ValueID) bool {
	for _, r := range results {
		if r.ValueID == id {
			return true
		}
	}
	return false
}

func assertSortedByScore(t *testing.T, results []Result) {
	t.Helper()
	for i := 1; i < len(results); i++ {
		if results[i-1].Score > results[i].Score {
			t.Fatalf("results not sorted by score: %+v", results)
		}
	}
}
