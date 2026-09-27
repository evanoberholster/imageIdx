//go:build !race

package search

import (
	"context"
	"encoding/binary"
	"testing"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
)

// Pool-backed query scratch may be discarded deliberately by the race
// detector, so allocation assertions belong only in ordinary builds.

// minAllocs takes the minimum across samples so rare process-wide noise
// (timers, background tasks counted by AllocsPerRun) cannot fail a
// steady-state allocation claim.
func minAllocs(samples, runs int, f func()) float64 {
	best := 1e9
	for n := 0; n < samples; n++ {
		if allocs := testing.AllocsPerRun(runs, f); allocs < best {
			best = allocs
		}
	}
	return best
}

// TestGrantsAddNoAllocations proves the grant helpers allocate nothing
// beyond the equivalent hand-rolled Index calls. Reads carry one
// 8-byte key alloc from bitmapidx's swappable hash seam (hashCanonical is
// a package variable so collision-injection tests can replace it, which
// forces the key heap-ward); the helper must match the manual baseline
// exactly. Store phases are quiesced with WaitIdle so background
// compactions from write churn cannot leak into a later sample.
func TestGrantsAddNoAllocations(t *testing.T) {
	ctx := context.Background()
	store := aclStore(t, true)
	userNamespace, ok := store.Lookup(common.NamespaceUserACL)
	if !ok {
		t.Fatal("missing user ACL namespace")
	}
	groupNamespace, ok := store.Lookup(common.NamespaceGroupACL)
	if !ok {
		t.Fatal("missing group ACL namespace")
	}
	users, err := NewUserGrants(userNamespace)
	if err != nil {
		t.Fatal(err)
	}
	groups, err := NewGroupGrants(groupNamespace)
	if err != nil {
		t.Fatal(err)
	}
	if err := users.Add(42, 1, 2, 3); err != nil {
		t.Fatal(err)
	}
	if err := groups.Add(7, 4); err != nil {
		t.Fatal(err)
	}

	stackUserValue := func(id uint64) bitmapidx.Value {
		var key [8]byte
		binary.LittleEndian.PutUint64(key[:], id)
		return bitmapidx.Value{Kind: common.KindBitmapEnum, Bytes: key[:]}
	}
	manualContains := func() { _, _ = userNamespace.Contains(1, stackUserValue(42)) }
	helperContains := func() { _, _ = users.Contains(42, 1) }
	if manual, helper := minAllocs(5, 100, manualContains), minAllocs(5, 100, helperContains); helper != manual {
		t.Fatalf("Contains helper allocs = %v, manual = %v", helper, manual)
	}
	manualGroupContains := func() { _, _ = groupNamespace.Contains(4, stackUserValue(7)) }
	helperGroupContains := func() { _, _ = groups.Contains(7, 4) }
	if manual, helper := minAllocs(5, 100, manualGroupContains), minAllocs(5, 100, helperGroupContains); helper != manual {
		t.Fatalf("group Contains helper allocs = %v, manual = %v", helper, manual)
	}

	ids := []ImageID{5, 6, 7}
	// Manual baselines assemble the batch exactly as a hand-rolled
	// caller would: one shared stack key, one stack entries array.
	manualAdd := func() {
		value := stackUserValue(42)
		var inline [8]bitmapidx.Entry
		entries := inline[:3]
		for n, image := range ids {
			entries[n] = bitmapidx.Entry{ImageID: image, Value: value}
		}
		batch := userNamespace.NewBatch()
		_ = batch.Add(entries...)
		_ = batch.Apply()
		batch.Close()
		_ = userNamespace.Barrier()
	}
	helperAdd := func() { _ = users.Add(42, ids...) }
	manualRemove := func() {
		value := stackUserValue(42)
		var inline [8]bitmapidx.Entry
		entries := inline[:3]
		for n, image := range ids {
			entries[n] = bitmapidx.Entry{ImageID: image, Value: value}
		}
		batch := userNamespace.NewBatch()
		_ = batch.Remove(entries...)
		_ = batch.Apply()
		batch.Close()
		_ = userNamespace.Barrier()
	}
	helperRemove := func() { _ = users.Remove(42, ids...) }
	// Each phase starts from identical membership (one real mutation plus
	// no-ops) and is quiesced first, so background compactions from write
	// churn cannot leak into a later sample.
	quiesce := func(present bool) {
		t.Helper()
		if present {
			_ = users.Add(42, ids...)
		} else {
			_ = users.Remove(42, ids...)
		}
		if err := userNamespace.WaitIdle(ctx); err != nil {
			t.Fatal(err)
		}
	}
	quiesce(false)
	manualAddAllocs := minAllocs(5, 100, manualAdd)
	quiesce(false)
	if helper := minAllocs(5, 100, helperAdd); helper != manualAddAllocs {
		t.Fatalf("Add helper allocs = %v, manual = %v", helper, manualAddAllocs)
	}
	quiesce(true)
	manualRemoveAllocs := minAllocs(5, 100, manualRemove)
	quiesce(true)
	if helper := minAllocs(5, 100, helperRemove); helper != manualRemoveAllocs {
		t.Fatalf("Remove helper allocs = %v, manual = %v", helper, manualRemoveAllocs)
	}
}
