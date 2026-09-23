package bitmapidx

import (
	"errors"
	"testing"
)

// aclFields builds the dedicated user and group fields: one bitmap per
// principal, each holding ImageIDs.
func aclFields(t *testing.T) (users, groups *FieldIndex) {
	t.Helper()
	users = mustField(t, enumField(IndexTypeUserACL), FieldConfig{})
	groups = mustField(t, enumField(IndexTypeGroupACL), FieldConfig{})
	return users, groups
}

func TestPrincipalValidation(t *testing.T) {
	if _, err := NewUserACL(0); !errors.Is(err, ErrInvalidPrincipal) {
		t.Fatalf("NewUserACL(0) err = %v, want ErrInvalidPrincipal", err)
	}
	if _, err := NewGroupACL(0); !errors.Is(err, ErrInvalidPrincipal) {
		t.Fatalf("NewGroupACL(0) err = %v, want ErrInvalidPrincipal", err)
	}
	// Only zero is rejected: every other ID, extremes included, is a
	// valid principal with no syntax to validate.
	for _, id := range []UserID{1, 42, ^UserID(0)} {
		if _, err := NewUserACL(id); err != nil {
			t.Fatalf("NewUserACL(%d) err = %v, want nil", id, err)
		}
		if _, err := NewGroupACL(GroupID(id)); err != nil {
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
	if value.Kind != KindEnum {
		t.Fatalf("user Value kind = %v, want KindEnum", value.Kind)
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
	// Equal IDs hash distinctly per field; there is no cross-namespace
	// confusion left to test — that property now belongs to the field.
	group, _ := NewGroupACL(42)
	if _, ok := ParseGroupACL(group.Value().Bytes); !ok {
		t.Fatal("group round trip failed")
	}
}

func TestUserACLGrantCheckRevoke(t *testing.T) {
	users, _ := aclFields(t)
	alice, err := NewUserACL(42)
	if err != nil {
		t.Fatal(err)
	}
	if err := users.Add(
		Entry{ImageID: 1, Value: alice.Value()},
		Entry{ImageID: 2, Value: alice.Value()},
	); err != nil {
		t.Fatal(err)
	}
	for _, id := range []ImageID{1, 2} {
		ok, err := users.Contains(id, alice.Value())
		if err != nil || !ok {
			t.Fatalf("Contains(%d) = %v, %v; want true, nil", id, ok, err)
		}
	}
	if ok, err := users.Contains(3, alice.Value()); err != nil || ok {
		t.Fatalf("Contains(3) = %v, %v; want false, nil", ok, err)
	}
	if _, ok, err := users.KeyForValue(alice.Value()); err != nil || !ok {
		t.Fatalf("KeyForValue = %v, %v; want resolved", ok, err)
	}
	if got := appendIDs(t, users, alice.Value()); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("ValueIDs = %v, want [1 2]", got)
	}
	if err := users.Remove(Entry{ImageID: 1, Value: alice.Value()}); err != nil {
		t.Fatal(err)
	}
	if ok, err := users.Contains(1, alice.Value()); err != nil || ok {
		t.Fatalf("Contains(1) after revoke = %v, %v; want false, nil", ok, err)
	}
	if got := appendIDs(t, users, alice.Value()); len(got) != 1 || got[0] != 2 {
		t.Fatalf("ValueIDs after revoke = %v, want [2]", got)
	}
}

func TestGroupGrantFlow(t *testing.T) {
	users, groups := aclFields(t)
	editors, err := NewGroupACL(7)
	if err != nil {
		t.Fatal(err)
	}
	alice, err := NewUserACL(42)
	if err != nil {
		t.Fatal(err)
	}
	bob, err := NewUserACL(43)
	if err != nil {
		t.Fatal(err)
	}
	// Direct user and group grants are separate postings. Search-time ACL
	// checks union a user's posting with their authenticated group postings.
	if err := users.Add(Entry{ImageID: 7, Value: alice.Value()}); err != nil {
		t.Fatal(err)
	}
	if err := groups.Add(Entry{ImageID: 7, Value: editors.Value()}); err != nil {
		t.Fatal(err)
	}
	if ok, err := users.Contains(7, bob.Value()); err != nil || ok {
		t.Fatalf("bob's direct grant = %v, %v; want false, nil", ok, err)
	}
	if got := appendIDs(t, groups, editors.Value()); len(got) != 1 || got[0] != 7 {
		t.Fatalf("group audit ValueIDs = %v, want [7]", got)
	}
	// Revoking a direct user grant leaves the group grant intact.
	if err := users.Remove(Entry{ImageID: 7, Value: alice.Value()}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := users.Contains(7, alice.Value()); ok {
		t.Fatal("revoked user grant still visible")
	}
	if got := appendIDs(t, groups, editors.Value()); len(got) != 1 || got[0] != 7 {
		t.Fatalf("group audit after revoke = %v, want [7]", got)
	}
}
