package search

import (
	"errors"
	"testing"

	common "github.com/evanoberholster/imageIdx/common"
)

func TestPrincipalValidation(t *testing.T) {
	if _, err := NewUserACL(0); !errors.Is(err, ErrInvalidPrincipal) {
		t.Fatalf("NewUserACL(0) err = %v, want ErrInvalidPrincipal", err)
	}
	if _, err := NewGroupACL(0); !errors.Is(err, ErrInvalidPrincipal) {
		t.Fatalf("NewGroupACL(0) err = %v, want ErrInvalidPrincipal", err)
	}
	// Only zero is rejected: every other ID, extremes included, is a
	// valid principal with no syntax to validate.
	for _, id := range []common.UserID{1, 42, ^common.UserID(0)} {
		if _, err := NewUserACL(id); err != nil {
			t.Fatalf("NewUserACL(%d) err = %v, want nil", id, err)
		}
		if _, err := NewGroupACL(common.GroupID(id)); err != nil {
			t.Fatalf("NewGroupACL(%d) err = %v, want nil", id, err)
		}
	}
	user, err := NewUserACL(42)
	if err != nil || user.ID() != 42 {
		t.Fatalf("NewUserACL(42) = %+v, %v", user, err)
	}
}

func TestPrincipalValueRoundTrip(t *testing.T) {
	user, _ := NewUserACL(42)
	value := user.Value()
	if value.Kind != common.KindBitmapEnum {
		t.Fatalf("user Value kind = %v, want KindBitmapEnum", value.Kind)
	}
	// Fixed 8-byte little-endian key.
	if len(value.Bytes) != 8 {
		t.Fatalf("user Value bytes len = %d, want 8", len(value.Bytes))
	}
	back, ok := ParseUserACL(value.Bytes)
	if !ok || back.ID() != 42 {
		t.Fatalf("ParseUserACL = %+v, %v", back, ok)
	}
	for _, garbage := range [][]byte{nil, {}, {1, 2, 3}, make([]byte, 9)} {
		if _, ok := ParseUserACL(garbage); ok {
			t.Fatalf("user parse accepted %d bytes", len(garbage))
		}
		if _, ok := ParseGroupACL(garbage); ok {
			t.Fatalf("group parse accepted %d bytes", len(garbage))
		}
	}
	// Equal IDs hash distinctly per namespace; hash domain separation over
	// the namespace number is what keeps them apart.
	group, _ := NewGroupACL(42)
	if _, ok := ParseGroupACL(group.Value().Bytes); !ok {
		t.Fatal("group round trip failed")
	}
}
