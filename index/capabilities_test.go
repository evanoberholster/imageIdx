package index

import (
	"testing"

	schema "github.com/evanoberholster/imageIdx/schema"
)

func TestFamilyCapabilityContract(t *testing.T) {
	for _, kind := range []schema.NamespaceKind{
		schema.KindTimestamp, schema.KindGeo, schema.KindHashExact,
		schema.KindBitmapEnum, schema.KindBitmapBool, schema.KindBitmapString,
		schema.KindBitmapContinuous, schema.KindBitmapBSI,
	} {
		caps := FamilyCapabilitiesFor(kind)
		if !caps.CoalesceSafe || !caps.DeleteByID || !caps.ReplayIdempotent || !caps.Batch {
			t.Fatalf("%s capabilities = %+v", kind, caps)
		}
	}
	if FamilyCapabilitiesFor(schema.KindHashPerceptual).CoalesceSafe {
		t.Fatal("SimHash coalescing must remain disabled")
	}
	if FamilyCapabilitiesFor(schema.KindVector).CoalesceSafe || FamilyCapabilitiesFor(schema.KindVector).Batch {
		t.Fatal("vector coalescing/batch must remain disabled")
	}
}
