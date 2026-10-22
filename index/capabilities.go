package index

import schema "github.com/evanoberholster/imageIdx/schema"

// FamilyCapabilities is the static contract for one index family. It is
// intentionally a value table, not configuration: changing a promise requires
// changing the engine and its tests.
type FamilyCapabilities struct {
	// CoalesceSafe permits replacing an ordered group of entity updates with
	// one typed before/after delta.
	CoalesceSafe bool
	// DeleteByID means an engine can durably remove all associations for an ID.
	DeleteByID bool
	// ReplayIdempotent means reapplying the same interval is harmless.
	ReplayIdempotent bool
	// Batch means the engine exposes a multi-record apply path.
	Batch bool
}

// FamilyCapabilitiesFor returns the conservative capability contract for kind.
func FamilyCapabilitiesFor(kind schema.NamespaceKind) FamilyCapabilities {
	switch kind {
	case schema.KindTimestamp, schema.KindGeo, schema.KindHashExact,
		schema.KindBitmapEnum, schema.KindBitmapBool, schema.KindBitmapString,
		schema.KindBitmapContinuous, schema.KindBitmapBSI:
		return FamilyCapabilities{CoalesceSafe: true, DeleteByID: true, ReplayIdempotent: true, Batch: true}
	case schema.KindHashPerceptual:
		// Sparse publication is source-generation idempotent, but SimHash
		// coalescing and unified delete-by-ID remain disabled.
		return FamilyCapabilities{CoalesceSafe: false, DeleteByID: false, ReplayIdempotent: true, Batch: true}
	case schema.KindVector:
		// Vector graph updates retain engine-local ordering and the unified
		// batch API is not enabled yet.
		return FamilyCapabilities{CoalesceSafe: false, DeleteByID: true, ReplayIdempotent: true, Batch: false}
	default:
		return FamilyCapabilities{}
	}
}

// CoalesceSafe reports whether the registered namespace family can participate
// in entity-level delta coalescing.
func (s *Store) CoalesceSafe(ns schema.Namespace) bool {
	rt, ok := s.reg.lookup(ns)
	if !ok {
		return false
	}
	return FamilyCapabilitiesFor(rt.kind).CoalesceSafe
}
