package database

import (
	"bytes"
	"testing"

	schema "github.com/evanoberholster/imageIdx/schema"
)

func testWorkspaceMutation(t *testing.T, ws *coordinatorWorkspace, sequence uint64, before, after []byte, deleted bool) queuedMutation {
	t.Helper()
	raw, err := encodeMutation(Mutation{
		Entity:     schema.EntityImage,
		ValueID:    sequence,
		Delete:     deleted,
		Projection: after,
	}, before)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := decodeMutation(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !ws.appendMutation(sequence, raw, entry) {
		t.Fatal("appendMutation rejected")
	}
	return ws.mutations[ws.count-1]
}

func TestCoordinatorWorkspaceCopiesOnceAndResolvesRefs(t *testing.T) {
	ws := newCoordinatorWorkspace(defaultMaxProjectionBytes, defaultOversizedRetentionBytes)
	ws.reset()
	before := bytes.Repeat([]byte{1}, 100)
	after := bytes.Repeat([]byte{2}, 200)
	m := testWorkspaceMutation(t, ws, 1, before, after, false)
	if !m.before.valid || !m.after.valid {
		t.Fatal("expected valid before/after refs")
	}
	if m.before.slot != m.after.slot {
		t.Fatalf("before/after should share one copied journal payload: %d vs %d", m.before.slot, m.after.slot)
	}
	if got := ws.refBytes(m.before); !bytes.Equal(got, before) {
		t.Fatalf("before ref = %v, want %v", got, before)
	}
	if got := ws.refBytes(m.after); !bytes.Equal(got, after) {
		t.Fatalf("after ref = %v, want %v", got, after)
	}
	// Mutating the borrowed journal bytes must not change the copied payload.
	before[0], after[0] = 9, 9
	if ws.refBytes(m.before)[0] != 1 || ws.refBytes(m.after)[0] != 2 {
		t.Fatal("workspace aliases borrowed journal bytes")
	}
}

func TestCoordinatorWorkspaceEmptyPayload(t *testing.T) {
	ws := newCoordinatorWorkspace(defaultMaxProjectionBytes, defaultOversizedRetentionBytes)
	ws.reset()
	m := testWorkspaceMutation(t, ws, 1, nil, nil, true)
	if m.before.valid || m.after.valid {
		t.Fatal("empty payload produced valid refs")
	}
	if ws.refBytes(m.before) != nil || ws.refBytes(m.after) != nil {
		t.Fatal("empty refs resolved to data")
	}
}

func TestCoordinatorWorkspaceByteBudget(t *testing.T) {
	ws := newCoordinatorWorkspace(10, defaultOversizedRetentionBytes)
	ws.reset()
	testWorkspaceMutation(t, ws, 1, bytes.Repeat([]byte{1}, 4), bytes.Repeat([]byte{2}, 4), false)
	raw, err := encodeMutation(Mutation{Entity: schema.EntityImage, ValueID: 2, Projection: bytes.Repeat([]byte{3}, 4)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := decodeMutation(raw)
	if err != nil {
		t.Fatal(err)
	}
	if ws.appendMutation(2, raw, entry) {
		t.Fatal("byte budget accepted an over-limit mutation")
	}
	if ws.count != 1 || ws.logicalBytes != 8 {
		t.Fatalf("workspace count=%d logical=%d", ws.count, ws.logicalBytes)
	}
}

func TestCoordinatorWorkspaceOversizedPayload(t *testing.T) {
	ws := newCoordinatorWorkspace(defaultMaxProjectionBytes, 2<<20)
	ws.reset()
	before := bytes.Repeat([]byte{1}, 70<<10)
	after := bytes.Repeat([]byte{2}, 70<<10)
	m := testWorkspaceMutation(t, ws, 1, before, after, false)
	if int(m.before.slot) >= len(ws.blocks) || ws.blocks[m.before.slot].ordinary {
		t.Fatal("oversized payload did not use an oversized slot")
	}
	if !bytes.Equal(ws.refBytes(m.before), before) || !bytes.Equal(ws.refBytes(m.after), after) {
		t.Fatal("oversized payload mismatch")
	}
	ws.reset()
	if len(ws.blocks) != 1 || ws.blocks[0].ordinary {
		t.Fatalf("expected one retained oversized block, got %d", len(ws.blocks))
	}
	ws.reset()
	// Above the retention ceiling, the oversized buffer is released.
	ws2 := newCoordinatorWorkspace(defaultMaxProjectionBytes, 1<<10)
	ws2.reset()
	testWorkspaceMutation(t, ws2, 1, bytes.Repeat([]byte{1}, 70<<10), []byte{2}, false)
	ws2.reset()
	if len(ws2.blocks) != 0 {
		t.Fatalf("oversized buffer above ceiling retained %d blocks", len(ws2.blocks))
	}
}

func TestCoordinatorWorkspaceStaleReferencePanics(t *testing.T) {
	ws := newCoordinatorWorkspace(defaultMaxProjectionBytes, defaultOversizedRetentionBytes)
	ws.reset()
	m := testWorkspaceMutation(t, ws, 1, bytes.Repeat([]byte{1}, 8), []byte{2}, false)
	ws.reset()
	defer func() {
		if recover() == nil {
			t.Fatal("stale projection reference did not panic")
		}
	}()
	_ = ws.refBytes(m.before)
}
