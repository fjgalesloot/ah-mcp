package tools

import (
	"encoding/json"
	"strings"
	"testing"

	appie "github.com/gwillem/appie-go"
)

func TestFulfillmentTotal(t *testing.T) {
	fs := []appie.Fulfillment{{OrderID: 1, TotalPrice: 0}, {OrderID: 2, TotalPrice: 186.74}}
	if got := fulfillmentTotal(fs, 2); got == nil || *got != 186.74 {
		t.Fatalf("total for order 2 = %v, want 186.74", got)
	}
	if got := fulfillmentTotal(fs, 1); got != nil {
		t.Fatalf("total for order 1 = %v, want nil when AH reports 0", *got)
	}
	if got := fulfillmentTotal(fs, 3); got != nil {
		t.Fatalf("total for unlisted order = %v, want nil", *got)
	}
}

// AH's details endpoint has no totals; ah_get_order_details used to report
// total_price 0 for every order because of it.
func TestOrderDetailsViewTotals(t *testing.T) {
	order := &appie.Order{ID: "2", Items: []appie.OrderItem{
		{ProductID: 10, Quantity: 2, Product: &appie.Product{Title: "Kaas", Price: appie.Price{Now: 3.00, Was: 4.50}}},
		{ProductID: 11, Quantity: 1, Product: &appie.Product{Title: "Melk", Price: appie.Price{Now: 1.19}}},
	}}
	total := 7.19

	b, err := json.Marshal(newOrderDetailsView(order, &total))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{`"total_price":7.19`, `"subtotal_before_bonus":10.19`, `"price":3,"price_before_bonus":4.5`} {
		if !strings.Contains(s, want) {
			t.Fatalf("payload %s missing %s", s, want)
		}
	}

	b, _ = json.Marshal(newOrderDetailsView(order, nil))
	if strings.Contains(string(b), "total_price") {
		t.Fatalf("payload %s should omit total_price when it is unknown", b)
	}
}
