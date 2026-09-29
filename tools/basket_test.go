package tools

import "testing"

func productItem(pid int, title, desc, itemType, origin string) v2ListItem {
	it := v2ListItem{Description: desc, Type: itemType, OriginCode: origin}
	it.ProductDetails.Product.WebshopID = pid
	it.ProductDetails.Product.Title = title
	return it
}

func TestV2ListItemName(t *testing.T) {
	if got := productItem(1, "Melk halfvol", "", "", "").name(); got != "Melk halfvol" {
		t.Fatalf("name = %q, want the product title when description is blank", got)
	}
	if got := productItem(1, "Melk halfvol", "verse bloemen", "", "").name(); got != "verse bloemen" {
		t.Fatalf("name = %q, want the description when it is set", got)
	}
	if got := (v2ListItem{}).name(); got != "" {
		t.Fatalf("name = %q, want empty for an item with neither", got)
	}
}

// AH rejects the removal PATCH when description, type or originCode are blank,
// so each has to fall back to what the app would have sent.
func TestRemovalPatchFillsRequiredFields(t *testing.T) {
	got := removalPatch(productItem(12345, "Melk halfvol", "", "", ""))

	if got.Quantity != 0 {
		t.Fatalf("Quantity = %d, want 0 (that is what signals deletion)", got.Quantity)
	}
	if got.ProductID != 12345 {
		t.Fatalf("ProductID = %d, want 12345", got.ProductID)
	}
	if got.Description != "Melk halfvol" {
		t.Fatalf("Description = %q, want the product title fallback", got.Description)
	}
	if got.SearchTerm != "Melk halfvol" {
		t.Fatalf("SearchTerm = %q, want it to mirror the description", got.SearchTerm)
	}
	if got.Type != "SHOPPABLE" {
		t.Fatalf("Type = %q, want the SHOPPABLE fallback", got.Type)
	}
	if got.OriginCode != "PRD" {
		t.Fatalf("OriginCode = %q, want the PRD fallback", got.OriginCode)
	}
	if got.StrikeThrough {
		t.Fatal("StrikeThrough should be false on removal")
	}
}

func TestRemovalPatchPreservesExistingFields(t *testing.T) {
	got := removalPatch(productItem(9, "Title", "verse bloemen", "FREE_TEXT", "MAN"))

	if got.Description != "verse bloemen" || got.SearchTerm != "verse bloemen" {
		t.Fatalf("description/searchTerm = %q/%q, want the original description", got.Description, got.SearchTerm)
	}
	if got.Type != "FREE_TEXT" {
		t.Fatalf("Type = %q, want the item's own type", got.Type)
	}
	if got.OriginCode != "MAN" {
		t.Fatalf("OriginCode = %q, want the item's own origin", got.OriginCode)
	}
}

// Free-text items carry no product id; omitempty must keep productId out of
// the payload rather than sending 0.
func TestRemovalPatchOmitsZeroProductID(t *testing.T) {
	got := removalPatch(v2ListItem{Description: "verse bloemen", Type: "FREE_TEXT", OriginCode: "MAN"})
	if got.ProductID != 0 {
		t.Fatalf("ProductID = %d, want 0 for a free-text item", got.ProductID)
	}
}

// The v2 PATCH sets quantities, so adding to a listed product must send the
// sum — sending the requested quantity alone left "add one more" a no-op.
func TestAddPatchesAddsToExistingQuantity(t *testing.T) {
	melk := productItem(111, "Melk halfvol", "", "SHOPPABLE", "PRD")
	melk.Quantity = 1
	list := []v2ListItem{melk}

	got := addPatches(list, []lineItem{{ProductID: 111, Quantity: 1}, {ProductID: 222, Quantity: 3}})
	if len(got) != 2 {
		t.Fatalf("patches = %+v, want 2", got)
	}
	if got[0].ProductID != 111 || got[0].Quantity != 2 {
		t.Fatalf("existing product patch = %+v, want product 111 at quantity 2 (1 listed + 1 added)", got[0])
	}
	if got[0].Description != "Melk halfvol" || got[0].Type != "SHOPPABLE" || got[0].OriginCode != "PRD" {
		t.Fatalf("existing product patch = %+v, want the listed item's fields kept", got[0])
	}
	if got[1].ProductID != 222 || got[1].Quantity != 3 || got[1].Type != "SHOPPABLE" || got[1].OriginCode != "PRD" {
		t.Fatalf("new product patch = %+v, want product 222 at quantity 3", got[1])
	}
}

func TestAddPatchesSumsDuplicateProducts(t *testing.T) {
	got := addPatches(nil, []lineItem{{ProductID: 5, Quantity: 1}, {ProductID: 6, Quantity: 1}, {ProductID: 5, Quantity: 2}})
	if len(got) != 2 || got[0].ProductID != 5 || got[0].Quantity != 3 || got[1].ProductID != 6 {
		t.Fatalf("patches = %+v, want product 5 once at quantity 3, then product 6", got)
	}
}

// A checked-off item was already picked up; adding it again means it is
// needed again, so it restarts from the requested quantity, unchecked.
func TestAddPatchesRestartsCheckedItems(t *testing.T) {
	done := productItem(7, "Brood", "", "SHOPPABLE", "PRD")
	done.Quantity = 4
	done.StrikedThrough = true

	got := addPatches([]v2ListItem{done}, []lineItem{{ProductID: 7, Quantity: 1}})
	if len(got) != 1 || got[0].Quantity != 1 || got[0].StrikeThrough {
		t.Fatalf("patches = %+v, want quantity 1 and unchecked", got)
	}
}
