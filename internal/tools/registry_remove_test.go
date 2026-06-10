package tools

import (
	"slices"
	"testing"
)

func TestRegistryRemoveDropsToolAndPreservesOrder(t *testing.T) {
	r := &Registry{}
	r.Register(newOK("alpha", "a"))
	r.Register(newOK("beta", "b"))
	r.Register(newOK("gamma", "c"))

	if ok := r.Remove("beta"); !ok {
		t.Fatalf("Remove(beta) = false, want true")
	}
	got := r.Names()
	want := []string{"alpha", "gamma"}
	if !slices.Equal(got, want) {
		t.Fatalf("Names after remove = %v, want %v", got, want)
	}
	// The removed tool must no longer dispatch or appear in Specs.
	if _, ok := r.tools["beta"]; ok {
		t.Errorf("beta still present in tools map after Remove")
	}
	if len(r.Specs()) != 2 {
		t.Errorf("Specs = %d, want 2 after remove", len(r.Specs()))
	}
}

func TestRegistryRemoveAbsentIsNoOp(t *testing.T) {
	r := &Registry{}
	r.Register(newOK("alpha", "a"))

	if ok := r.Remove("missing"); ok {
		t.Fatalf("Remove(missing) = true, want false")
	}
	if got := r.Names(); !slices.Equal(got, []string{"alpha"}) {
		t.Fatalf("Names after no-op remove = %v, want [alpha]", got)
	}
}

func TestRegistryRemoveOnEmptyRegistry(t *testing.T) {
	r := &Registry{}
	if ok := r.Remove("anything"); ok {
		t.Fatalf("Remove on empty registry = true, want false")
	}
}

func TestRegistryRemoveThenReRegisterAppendsAtEnd(t *testing.T) {
	r := &Registry{}
	r.Register(newOK("alpha", "a"))
	r.Register(newOK("beta", "b"))
	r.Remove("alpha")
	r.Register(newOK("alpha", "a2"))
	// alpha was removed then re-added, so it lands at the end of the order.
	if got := r.Names(); !slices.Equal(got, []string{"beta", "alpha"}) {
		t.Fatalf("Names = %v, want [beta alpha]", got)
	}
}
