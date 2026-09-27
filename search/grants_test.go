package search

import (
	"errors"
	"testing"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
)

func grantHandles(t *testing.T) (users UserGrants, groups GroupGrants, userNamespace *bitmapidx.Index) {
	t.Helper()
	store := aclStore(t, true)
	userNamespace, ok := store.Lookup(common.NamespaceUserACL)
	if !ok {
		t.Fatal("missing user ACL namespace")
	}
	groupNamespace, ok := store.Lookup(common.NamespaceGroupACL)
	if !ok {
		t.Fatal("missing group ACL namespace")
	}
	var err error
	if users, err = NewUserGrants(userNamespace); err != nil {
		t.Fatal(err)
	}
	if groups, err = NewGroupGrants(groupNamespace); err != nil {
		t.Fatal(err)
	}
	return users, groups, userNamespace
}

func TestGrantNamespaceValidation(t *testing.T) {
	if _, err := NewUserGrants(nil); !errors.Is(err, ErrInvalidACLField) {
		t.Fatalf("NewUserGrants(nil) err = %v, want ErrInvalidACLField", err)
	}
	if _, err := NewGroupGrants(nil); !errors.Is(err, ErrInvalidACLField) {
		t.Fatalf("NewGroupGrants(nil) err = %v, want ErrInvalidACLField", err)
	}
	store := aclStore(t, true)
	tags, _ := store.Lookup(common.NamespaceTag)
	if _, err := NewUserGrants(tags); !errors.Is(err, ErrInvalidACLField) {
		t.Fatalf("NewUserGrants(tag namespace) err = %v, want ErrInvalidACLField", err)
	}
}

func TestGrantNamespaceCrossWiring(t *testing.T) {
	store := aclStore(t, true)
	userNamespace, _ := store.Lookup(common.NamespaceUserACL)
	groupNamespace, _ := store.Lookup(common.NamespaceGroupACL)
	if _, err := NewUserGrants(groupNamespace); !errors.Is(err, ErrInvalidACLField) {
		t.Fatalf("NewUserGrants(group namespace) err = %v, want ErrInvalidACLField", err)
	}
	if _, err := NewGroupGrants(userNamespace); !errors.Is(err, ErrInvalidACLField) {
		t.Fatalf("NewGroupGrants(user namespace) err = %v, want ErrInvalidACLField", err)
	}
}

func TestUserGrantsAddContainsRemove(t *testing.T) {
	users, _, _ := grantHandles(t)
	if err := users.Add(42, 1, 2); err != nil {
		t.Fatal(err)
	}
	for _, id := range []ImageID{1, 2} {
		ok, err := users.Contains(42, id)
		if err != nil || !ok {
			t.Fatalf("Contains(42, %d) = %v, %v; want true, nil", id, ok, err)
		}
	}
	if ok, err := users.Contains(42, 3); err != nil || ok {
		t.Fatalf("Contains(42, 3) = %v, %v; want false, nil", ok, err)
	}
	if ok, err := users.Contains(43, 1); err != nil || ok {
		t.Fatalf("Contains(43, 1) = %v, %v; want false, nil", ok, err)
	}
	// Empty batches are no-ops.
	if err := users.Add(42); err != nil {
		t.Fatal(err)
	}
	if err := users.Remove(42); err != nil {
		t.Fatal(err)
	}
	if err := users.Remove(42, 1); err != nil {
		t.Fatal(err)
	}
	if ok, err := users.Contains(42, 1); err != nil || ok {
		t.Fatalf("Contains(42, 1) after revoke = %v, %v; want false, nil", ok, err)
	}
	if ok, err := users.Contains(42, 2); err != nil || !ok {
		t.Fatalf("Contains(42, 2) after partial revoke = %v, %v; want true, nil", ok, err)
	}
}

func TestGrantsRejectZeroPrincipal(t *testing.T) {
	users, groups, _ := grantHandles(t)
	if err := users.Add(0, 1); !errors.Is(err, ErrInvalidPrincipal) {
		t.Fatalf("users.Add(0, 1) err = %v, want ErrInvalidPrincipal", err)
	}
	if _, err := users.Contains(0, 1); !errors.Is(err, ErrInvalidPrincipal) {
		t.Fatalf("users.Contains(0, 1) err = %v, want ErrInvalidPrincipal", err)
	}
	if err := users.Remove(0, 1); !errors.Is(err, ErrInvalidPrincipal) {
		t.Fatalf("users.Remove(0, 1) err = %v, want ErrInvalidPrincipal", err)
	}
	if err := groups.Add(0, 1); !errors.Is(err, ErrInvalidPrincipal) {
		t.Fatalf("groups.Add(0, 1) err = %v, want ErrInvalidPrincipal", err)
	}
	if _, err := groups.Contains(0, 1); !errors.Is(err, ErrInvalidPrincipal) {
		t.Fatalf("groups.Contains(0, 1) err = %v, want ErrInvalidPrincipal", err)
	}
	if err := groups.Remove(0, 1); !errors.Is(err, ErrInvalidPrincipal) {
		t.Fatalf("groups.Remove(0, 1) err = %v, want ErrInvalidPrincipal", err)
	}
}

func TestGroupGrantsFlow(t *testing.T) {
	users, groups, _ := grantHandles(t)
	if err := users.Add(42, 7); err != nil {
		t.Fatal(err)
	}
	if err := groups.Add(7, 7); err != nil {
		t.Fatal(err)
	}
	// Direct user and group grants are separate postings.
	if ok, err := users.Contains(43, 7); err != nil || ok {
		t.Fatalf("bob's direct grant = %v, %v; want false, nil", ok, err)
	}
	if ok, err := groups.Contains(7, 7); err != nil || !ok {
		t.Fatalf("groups.Contains(7, 7) = %v, %v; want true, nil", ok, err)
	}
	// Revoking a direct user grant leaves the group grant intact.
	if err := users.Remove(42, 7); err != nil {
		t.Fatal(err)
	}
	if ok, _ := users.Contains(42, 7); ok {
		t.Fatal("revoked user grant still visible")
	}
	if ok, err := groups.Contains(7, 7); err != nil || !ok {
		t.Fatalf("group grant after revoke = %v, %v; want true, nil", ok, err)
	}
}

func TestGrantsLargeBatch(t *testing.T) {
	users, _, _ := grantHandles(t)
	ids := make([]ImageID, 0, 20)
	for id := ImageID(1); id <= 20; id++ {
		ids = append(ids, id)
	}
	if err := users.Add(42, ids...); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if ok, err := users.Contains(42, id); err != nil || !ok {
			t.Fatalf("Contains(42, %d) = %v, %v; want true, nil", id, ok, err)
		}
	}
	if err := users.Remove(42, ids...); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if ok, err := users.Contains(42, id); err != nil || ok {
			t.Fatalf("Contains(42, %d) after revoke = %v, %v; want false, nil", id, ok, err)
		}
	}
}
