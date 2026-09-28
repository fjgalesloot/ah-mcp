package tools

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"sync"
	"time"

	appie "github.com/gwillem/appie-go"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

const (
	// frequentItemsMaxOrders caps how much history ah_get_frequent_items walks.
	// Each order costs one API call, so an unbounded history turns a single
	// tool call into hundreds of requests.
	frequentItemsMaxOrders   = 25
	frequentItemsHardMax     = 100
	frequentItemsConcurrency = 4
)

// RegisterOrderTools registers order-related MCP tools.
func RegisterOrderTools(s *server.MCPServer, deps Deps) {
	registerGetOrderHistory(s, deps)
	registerGetPastOrders(s, deps)
	registerGetOrderDetails(s, deps)
	registerGetFrequentItems(s, deps)
	registerGetReceipts(s, deps)
	registerGetReceiptDetails(s, deps)
	registerGetCart(s, deps)
	registerGetCartSummary(s, deps)
	registerUpdateCartItem(s, deps)
	registerRemoveFromCart(s, deps)
	registerClearCart(s, deps)
	registerReopenOrder(s, deps)
	registerUpdateOrderItems(s, deps)
	registerRevertOrder(s, deps)
}

// --- ah_get_order_history ---

func registerGetOrderHistory(s *server.MCPServer, deps Deps) {
	tool := mcp.NewTool("ah_get_order_history",
		mcp.WithTitleAnnotation("Albert Heijn: Order History"),
		mcp.WithDescription(
			"Get upcoming Albert Heijn online delivery orders (open fulfillments). "+
				"Returns id, date, total_price, status, modifiable flag. "+
				"Use the returned id with ah_reopen_order to edit a submitted order before its closing time.",
		),
		mcp.WithString("limit",
			mcp.Description("Maximum number of orders to return (default 10, max 100)"),
		),
	)
	s.AddTool(tool, withClient(deps, func(ctx context.Context, c *appie.Client, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		limit := clamp(req.GetInt("limit", 10), 1, 100)

		var fulfillments []appie.Fulfillment
		if err := withRetry(ctx, "ah_get_order_history", func() error {
			var e error
			fulfillments, e = c.GetFulfillments(ctx)
			return e
		}); err != nil {
			return errResult(fmt.Sprintf("Failed to get order history: %v", err)), nil
		}

		type orderEntry struct {
			ID           int     `json:"id"`
			Date         string  `json:"date,omitempty"`
			TimeWindow   string  `json:"time_window,omitempty"`
			TotalPrice   float64 `json:"total_price"`
			ItemCount    int     `json:"item_count,omitempty"`
			Status       string  `json:"status"`
			ShoppingType string  `json:"shopping_type,omitempty"`
			Modifiable   bool    `json:"modifiable"`
		}
		results := make([]orderEntry, 0, min(limit, len(fulfillments)))
		for i, f := range fulfillments {
			if i >= limit {
				break
			}
			date := f.Delivery.Slot.DateDisplay
			if date == "" {
				date = f.Delivery.Slot.Date
			}
			results = append(results, orderEntry{
				ID:           f.OrderID,
				Date:         date,
				TimeWindow:   f.Delivery.Slot.TimeDisplay,
				TotalPrice:   f.TotalPrice,
				Status:       f.StatusDescription,
				ShoppingType: f.ShoppingType,
				Modifiable:   f.Modifiable,
			})
		}
		return jsonResult(results)
	}))
}

// closedFulfillmentsQuery lists delivered orders. The open ones come from the
// REST fulfillments endpoint; CLOSED is only exposed over GraphQL.
const closedFulfillmentsQuery = `query OrderFulfillmentsClosed {
  orderFulfillments(status: CLOSED) {
    result {
      orderId
      statusCode
      statusDescription
      shoppingType
      transactionCompleted
      totalPrice {
        totalPrice { amount }
      }
      delivery {
        status
        slot {
          date
          dateDisplay
          timeDisplay
        }
      }
    }
  }
}`

type closedFulfillment struct {
	OrderID           int    `json:"orderId"`
	StatusDescription string `json:"statusDescription"`
	ShoppingType      string `json:"shoppingType"`
	TotalPrice        struct {
		TotalPrice struct {
			Amount float64 `json:"amount"`
		} `json:"totalPrice"`
	} `json:"totalPrice"`
	Delivery struct {
		Status string `json:"status"`
		Slot   struct {
			Date        string `json:"date"`
			DateDisplay string `json:"dateDisplay"`
			TimeDisplay string `json:"timeDisplay"`
		} `json:"slot"`
	} `json:"delivery"`
}

type closedFulfillmentsResponse struct {
	OrderFulfillments struct {
		Result []closedFulfillment `json:"result"`
	} `json:"orderFulfillments"`
}

// --- ah_get_past_orders ---

func registerGetPastOrders(s *server.MCPServer, deps Deps) {
	tool := mcp.NewTool("ah_get_past_orders",
		mcp.WithTitleAnnotation("Albert Heijn: Past Orders"),
		mcp.WithDescription(
			"Get past/delivered Albert Heijn online delivery orders. "+
				"Returns id, date, total_price, status. "+
				"Use the returned id with ah_get_order_details to see full item lists.",
		),
		mcp.WithString("limit",
			mcp.Description("Maximum number of orders to return (default 10, max 100)"),
		),
	)
	s.AddTool(tool, withClient(deps, func(ctx context.Context, c *appie.Client, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		limit := clamp(req.GetInt("limit", 10), 1, 100)

		var resp closedFulfillmentsResponse
		if err := withRetry(ctx, "ah_get_past_orders", func() error {
			return c.DoGraphQL(ctx, closedFulfillmentsQuery, nil, &resp)
		}); err != nil {
			return errResult(fmt.Sprintf("Failed to get past orders: %v", err)), nil
		}

		type orderEntry struct {
			ID           int     `json:"id"`
			Date         string  `json:"date,omitempty"`
			TimeWindow   string  `json:"time_window,omitempty"`
			TotalPrice   float64 `json:"total_price"`
			Status       string  `json:"status"`
			ShoppingType string  `json:"shopping_type,omitempty"`
		}
		results := make([]orderEntry, 0)
		for i, f := range resp.OrderFulfillments.Result {
			if i >= limit {
				break
			}
			date := f.Delivery.Slot.DateDisplay
			if date == "" {
				date = f.Delivery.Slot.Date
			}
			results = append(results, orderEntry{
				ID:           f.OrderID,
				Date:         date,
				TimeWindow:   f.Delivery.Slot.TimeDisplay,
				TotalPrice:   f.TotalPrice.TotalPrice.Amount,
				Status:       f.StatusDescription,
				ShoppingType: f.ShoppingType,
			})
		}
		if len(results) == 0 {
			return mcp.NewToolResultText("No past orders found."), nil
		}
		return jsonResult(results)
	}))
}

// --- ah_get_frequent_items ---

func registerGetFrequentItems(s *server.MCPServer, deps Deps) {
	tool := mcp.NewTool("ah_get_frequent_items",
		mcp.WithTitleAnnotation("Albert Heijn: Frequently Ordered Items"),
		mcp.WithDescription(
			"Get frequently ordered products by analysing order history. "+
				"Expands the most recent orders, counts per product, and returns products "+
				"ordered at least min_order_count times. "+
				"Returns product_name, product_id, order_count, last_ordered_date.",
		),
		mcp.WithString("min_order_count",
			mcp.Description("Minimum number of orders a product must appear in (default 3)"),
		),
		mcp.WithString("max_orders",
			mcp.Description("How many recent orders to analyse (default 25, max 100). Each order costs one API call."),
		),
	)
	s.AddTool(tool, withClient(deps, func(ctx context.Context, c *appie.Client, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		minCount := clamp(req.GetInt("min_order_count", 3), 1, 1000)
		maxOrders := clamp(req.GetInt("max_orders", frequentItemsMaxOrders), 1, frequentItemsHardMax)
		start := time.Now()

		// Fetch both open and past (CLOSED) fulfillments so we have a full history.
		var openFulfillments []appie.Fulfillment
		if err := withRetry(ctx, "ah_get_frequent_items", func() error {
			var e error
			openFulfillments, e = c.GetFulfillments(ctx)
			return e
		}); err != nil {
			return errResult(fmt.Sprintf("Failed to get open fulfillments: %v", err)), nil
		}

		var closed closedFulfillmentsResponse
		if err := c.DoGraphQL(ctx, closedFulfillmentsQuery, nil, &closed); err != nil {
			// CLOSED history is best-effort — an account may have none.
			LogWarn("ah_get_frequent_items", "closed fulfillments unavailable: %v", err)
		}

		type minFulfillment struct {
			OrderID   int
			OrderDate string
		}
		var allFulfillments []minFulfillment
		for _, f := range openFulfillments {
			allFulfillments = append(allFulfillments, minFulfillment{OrderID: f.OrderID, OrderDate: f.Delivery.Slot.Date})
		}
		for _, f := range closed.OrderFulfillments.Result {
			allFulfillments = append(allFulfillments, minFulfillment{OrderID: f.OrderID, OrderDate: f.Delivery.Slot.Date})
		}

		// Newest first, then cap: analysing every order ever placed would fan
		// out into hundreds of sequential API calls.
		sort.SliceStable(allFulfillments, func(i, j int) bool {
			return allFulfillments[i].OrderDate > allFulfillments[j].OrderDate
		})
		truncated := false
		if len(allFulfillments) > maxOrders {
			allFulfillments = allFulfillments[:maxOrders]
			truncated = true
		}

		type productStats struct {
			Name          string
			Count         int
			LastOrderDate string
		}
		var (
			mu      sync.Mutex
			stats   = map[int]*productStats{}
			skipped int
			wg      sync.WaitGroup
			sem     = make(chan struct{}, frequentItemsConcurrency)
		)

		for _, f := range allFulfillments {
			wg.Add(1)
			go func(orderID int, orderDate string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()

				var order *appie.Order
				if err := withRetry(ctx, "ah_get_frequent_items", func() error {
					var e error
					order, e = c.GetOrderDetails(ctx, orderID)
					return e
				}); err != nil {
					LogWarn("ah_get_frequent_items", "could not fetch order %d: %v", orderID, err)
					mu.Lock()
					skipped++
					mu.Unlock()
					return
				}

				seen := map[int]bool{}
				mu.Lock()
				defer mu.Unlock()
				for _, item := range order.Items {
					pid := item.ProductID
					if pid <= 0 || seen[pid] {
						continue
					}
					seen[pid] = true

					name := ""
					if item.Product != nil {
						name = item.Product.Title
					}
					if name == "" {
						name = strconv.Itoa(pid)
					}

					if stats[pid] == nil {
						stats[pid] = &productStats{Name: name}
					}
					stats[pid].Count++
					// Keep the name from the most recent order we have seen.
					if orderDate > stats[pid].LastOrderDate {
						stats[pid].LastOrderDate = orderDate
						stats[pid].Name = name
					}
				}
			}(f.OrderID, f.OrderDate)
		}
		wg.Wait()

		type item struct {
			ProductName     string `json:"product_name"`
			ProductID       int    `json:"product_id"`
			OrderCount      int    `json:"order_count"`
			LastOrderedDate string `json:"last_ordered_date,omitempty"`
		}
		results := []item{}
		for pid, st := range stats {
			if st.Count >= minCount {
				results = append(results, item{
					ProductName:     st.Name,
					ProductID:       pid,
					OrderCount:      st.Count,
					LastOrderedDate: st.LastOrderDate,
				})
			}
		}
		sort.Slice(results, func(i, j int) bool {
			if results[i].OrderCount != results[j].OrderCount {
				return results[i].OrderCount > results[j].OrderCount
			}
			return results[i].ProductID < results[j].ProductID
		})

		LogInfo("ah_get_frequent_items", "orders=%d skipped=%d products=%d duration=%v",
			len(allFulfillments), skipped, len(results), time.Since(start))

		type payload struct {
			OrdersAnalysed int    `json:"orders_analysed"`
			OrdersSkipped  int    `json:"orders_skipped,omitempty"`
			Truncated      bool   `json:"truncated,omitempty"`
			Note           string `json:"note,omitempty"`
			Items          []item `json:"items"`
		}
		out := payload{
			OrdersAnalysed: len(allFulfillments),
			OrdersSkipped:  skipped,
			Truncated:      truncated,
			Items:          results,
		}
		if truncated {
			out.Note = fmt.Sprintf("Only the %d most recent orders were analysed. Raise max_orders for a longer history.", maxOrders)
		}
		return jsonResult(out)
	}))
}

// --- ah_get_receipts ---

func registerGetReceipts(s *server.MCPServer, deps Deps) {
	tool := mcp.NewTool("ah_get_receipts",
		mcp.WithTitleAnnotation("Albert Heijn: Receipts"),
		mcp.WithDescription(
			"List recent Albert Heijn in-store receipts (kassabonnen). "+
				"Returns receipt id, date, and total amount. "+
				"Use ah_get_receipt_details with the id to see individual items.",
		),
		mcp.WithString("limit",
			mcp.Description("Maximum number of receipts to return (default 10, max 100)"),
		),
	)
	s.AddTool(tool, withClient(deps, func(ctx context.Context, c *appie.Client, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		limit := clamp(req.GetInt("limit", 10), 1, 100)

		var receipts []appie.Receipt
		if err := withRetry(ctx, "ah_get_receipts", func() error {
			var e error
			receipts, e = c.GetReceipts(ctx)
			return e
		}); err != nil {
			return errResult(fmt.Sprintf("Failed to get receipts: %v", err)), nil
		}

		type entry struct {
			ID          string  `json:"id"`
			Date        string  `json:"date"`
			TotalAmount float64 `json:"total_amount"`
		}
		results := make([]entry, 0, min(limit, len(receipts)))
		for i, r := range receipts {
			if i >= limit {
				break
			}
			// Reformat ISO datetime to readable date
			date := r.Date
			if t, err := time.Parse(time.RFC3339, date); err == nil {
				date = t.Format("2006-01-02 15:04")
			}
			results = append(results, entry{
				ID:          r.TransactionID,
				Date:        date,
				TotalAmount: r.TotalAmount,
			})
		}
		return jsonResult(results)
	}))
}

// --- ah_get_receipt_details ---

func registerGetReceiptDetails(s *server.MCPServer, deps Deps) {
	tool := mcp.NewTool("ah_get_receipt_details",
		mcp.WithTitleAnnotation("Albert Heijn: Receipt Details"),
		mcp.WithDescription(
			"Get full details of a single Albert Heijn in-store receipt (kassabon) by its id. "+
				"Returns all purchased items with name, quantity, unit price and line total, "+
				"plus any discounts and payment method. "+
				"Get the id from ah_get_receipts first.",
		),
		mcp.WithString("id",
			mcp.Required(),
			mcp.Description("Receipt transaction ID from ah_get_receipts"),
		),
	)
	s.AddTool(tool, withClient(deps, func(ctx context.Context, c *appie.Client, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("id", "")
		if id == "" {
			return errResult("id is required"), nil
		}

		var receipt *appie.Receipt
		if err := withRetry(ctx, "ah_get_receipt_details", func() error {
			var e error
			receipt, e = c.GetReceipt(ctx, id)
			return e
		}); err != nil {
			return errResult(fmt.Sprintf("Failed to get receipt %s: %v", id, err)), nil
		}

		type itemEntry struct {
			Name      string  `json:"name"`
			Quantity  int     `json:"quantity,omitempty"`
			UnitPrice float64 `json:"unit_price,omitempty"`
			Total     float64 `json:"total"`
		}
		type discountEntry struct {
			Name   string  `json:"name"`
			Amount float64 `json:"amount"`
		}
		type paymentEntry struct {
			Method string  `json:"method"`
			Amount float64 `json:"amount"`
		}
		type result struct {
			ID        string          `json:"id"`
			Items     []itemEntry     `json:"items"`
			Discounts []discountEntry `json:"discounts,omitempty"`
			Payments  []paymentEntry  `json:"payments,omitempty"`
		}

		items := make([]itemEntry, 0, len(receipt.Items))
		for _, it := range receipt.Items {
			items = append(items, itemEntry{
				Name:      it.Description,
				Quantity:  it.Quantity,
				UnitPrice: it.UnitPrice,
				Total:     it.Amount,
			})
		}
		discounts := make([]discountEntry, 0, len(receipt.Discounts))
		for _, d := range receipt.Discounts {
			discounts = append(discounts, discountEntry{Name: d.Name, Amount: d.Amount})
		}
		payments := make([]paymentEntry, 0, len(receipt.Payments))
		for _, p := range receipt.Payments {
			payments = append(payments, paymentEntry{Method: p.Method, Amount: p.Amount})
		}

		return jsonResult(result{
			ID:        receipt.TransactionID,
			Items:     items,
			Discounts: discounts,
			Payments:  payments,
		})
	}))
}

// orderItemEntry is the shared item shape for cart and order views.
type orderItemEntry struct {
	ProductID int     `json:"product_id"`
	Name      string  `json:"name,omitempty"`
	Quantity  int     `json:"quantity"`
	Price     float64 `json:"price,omitempty"`
	// PriceBeforeBonus is set when Price is a bonus price.
	PriceBeforeBonus float64 `json:"price_before_bonus,omitempty"`
}

// orderView is the shared payload for ah_get_cart and ah_get_order_details.
type orderView struct {
	ID            string           `json:"id"`
	State         string           `json:"state"`
	Items         []orderItemEntry `json:"items"`
	TotalPrice    float64          `json:"total_price"`
	TotalDiscount float64          `json:"total_discount,omitempty"`
}

func newOrderView(order *appie.Order) orderView {
	items := make([]orderItemEntry, 0, len(order.Items))
	for _, it := range order.Items {
		e := orderItemEntry{ProductID: it.ProductID, Quantity: it.Quantity}
		if it.Product != nil {
			e.Name = it.Product.Title
			e.Price = it.Product.Price.Now
			e.PriceBeforeBonus = it.Product.Price.Was
		}
		items = append(items, e)
	}
	return orderView{
		ID:            order.ID,
		State:         order.State,
		Items:         items,
		TotalPrice:    order.TotalPrice,
		TotalDiscount: order.TotalDiscount,
	}
}

// orderDetailsView is the ah_get_order_details payload. AH's details endpoint
// carries no totals, so TotalPrice (which shadows orderView's) comes from the
// order's fulfillment and is omitted when unknown rather than reported as 0.
type orderDetailsView struct {
	orderView
	TotalPrice          *float64 `json:"total_price,omitempty"`
	SubtotalBeforeBonus float64  `json:"subtotal_before_bonus"`
}

func newOrderDetailsView(order *appie.Order, total *float64) orderDetailsView {
	return orderDetailsView{
		orderView:           newOrderView(order),
		TotalPrice:          total,
		SubtotalBeforeBonus: math.Round(order.Subtotal()*100) / 100,
	}
}

// fulfillmentTotal looks up an order's payable total among the fulfillments.
// It returns nil when the order is not listed or AH reports no total for it.
func fulfillmentTotal(fulfillments []appie.Fulfillment, orderID int) *float64 {
	for _, f := range fulfillments {
		if f.OrderID == orderID && f.TotalPrice > 0 {
			total := f.TotalPrice
			return &total
		}
	}
	return nil
}

// --- ah_get_order_details ---

func registerGetOrderDetails(s *server.MCPServer, deps Deps) {
	tool := mcp.NewTool("ah_get_order_details",
		mcp.WithTitleAnnotation("Albert Heijn: Order Details"),
		mcp.WithDescription(
			"Get the full item list for a specific Albert Heijn delivery order by its ID. "+
				"Returns all products with names, quantities and unit prices (price is the bonus price when price_before_bonus is set), "+
				"subtotal_before_bonus, and total_price (the amount payable, omitted when AH does not report one). "+
				"Get order_id from ah_get_order_history.",
		),
		mcp.WithString("order_id",
			mcp.Required(),
			mcp.Description("Numeric order ID from ah_get_order_history"),
		),
	)
	s.AddTool(tool, withClient(deps, func(ctx context.Context, c *appie.Client, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		orderID := req.GetInt("order_id", 0)
		if orderID <= 0 {
			return errResult("order_id is required"), nil
		}

		var order *appie.Order
		if err := withRetry(ctx, "ah_get_order_details", func() error {
			var e error
			order, e = c.GetOrderDetails(ctx, orderID)
			return e
		}); err != nil {
			return errResult(fmt.Sprintf("Failed to get order details for %d: %v", orderID, err)), nil
		}

		// The details endpoint has no totals; the fulfillment list does.
		var total *float64
		if fulfillments, fErr := c.GetFulfillments(ctx); fErr == nil {
			total = fulfillmentTotal(fulfillments, orderID)
		} else {
			LogWarn("ah_get_order_details", "fetch fulfillments for total: %v", fErr)
		}
		return jsonResult(newOrderDetailsView(order, total))
	}))
}

// --- ah_get_cart ---

func registerGetCart(s *server.MCPServer, deps Deps) {
	tool := mcp.NewTool("ah_get_cart",
		mcp.WithTitleAnnotation("Albert Heijn: View Cart"),
		mcp.WithDescription(
			"View the current Albert Heijn online shopping cart (active order). "+
				"Returns the order state, all items with names and quantities, "+
				"total price, and total discount. "+
				"Use ah_update_cart_item or ah_remove_from_cart to modify items.",
		),
	)
	s.AddTool(tool, withClient(deps, func(ctx context.Context, c *appie.Client, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var order *appie.Order
		if err := withRetry(ctx, "ah_get_cart", func() error {
			var e error
			order, e = c.GetOrder(ctx)
			return e
		}); err != nil {
			return errResult(fmt.Sprintf("Failed to get cart: %v", err)), nil
		}
		return jsonResult(newOrderView(order))
	}))
}

// --- ah_get_cart_summary ---

func registerGetCartSummary(s *server.MCPServer, deps Deps) {
	tool := mcp.NewTool("ah_get_cart_summary",
		mcp.WithTitleAnnotation("Albert Heijn: Cart Summary"),
		mcp.WithDescription(
			"Get the Albert Heijn shopping cart totals: number of items, "+
				"total price, discount amount, and delivery cost.",
		),
	)
	s.AddTool(tool, withClient(deps, func(ctx context.Context, c *appie.Client, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var summary *appie.OrderSummary
		if err := withRetry(ctx, "ah_get_cart_summary", func() error {
			var e error
			summary, e = c.GetOrderSummary(ctx)
			return e
		}); err != nil {
			return errResult(fmt.Sprintf("Failed to get cart summary: %v", err)), nil
		}
		return jsonResult(summary)
	}))
}

// --- ah_update_cart_item ---

func registerUpdateCartItem(s *server.MCPServer, deps Deps) {
	tool := mcp.NewTool("ah_update_cart_item",
		mcp.WithTitleAnnotation("Albert Heijn: Update Cart Item"),
		mcp.WithDescription(
			"Set the quantity of a product in the Albert Heijn shopping cart. "+
				"Use product_id from ah_search_products or ah_get_cart. "+
				"Set quantity=0 to remove the item (or use ah_remove_from_cart).",
		),
		mcp.WithString("product_id",
			mcp.Required(),
			mcp.Description("Numeric product ID"),
		),
		mcp.WithString("quantity",
			mcp.Required(),
			mcp.Description("New quantity (0 removes the item)"),
		),
	)
	s.AddTool(tool, withClient(deps, func(ctx context.Context, c *appie.Client, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		productID := req.GetInt("product_id", 0)
		if productID <= 0 {
			return errResult("product_id is required"), nil
		}
		quantity := req.GetInt("quantity", -1)
		if quantity < 0 {
			return errResult("quantity is required and must be >= 0"), nil
		}

		if err := ensureActiveOrder(ctx, c, "ah_update_cart_item"); err != nil {
			return errResult(fmt.Sprintf("Failed to get active order: %v", err)), nil
		}

		if err := withRetry(ctx, "ah_update_cart_item", func() error {
			return c.UpdateOrderItem(ctx, productID, quantity)
		}); err != nil {
			return errResult(fmt.Sprintf("Failed to update cart item %d: %v", productID, err)), nil
		}
		if quantity == 0 {
			return mcp.NewToolResultText(fmt.Sprintf("Product %d removed from cart.", productID)), nil
		}
		return mcp.NewToolResultText(fmt.Sprintf("Product %d quantity set to %d.", productID, quantity)), nil
	}))
}

// ensureActiveOrder fetches the current order so the client caches its ID,
// which it sends as the appie-current-order-id header on write requests.
func ensureActiveOrder(ctx context.Context, c *appie.Client, tool string) error {
	return withRetry(ctx, tool, func() error {
		_, err := c.GetOrder(ctx)
		return err
	})
}

// --- ah_remove_from_cart ---

func registerRemoveFromCart(s *server.MCPServer, deps Deps) {
	tool := mcp.NewTool("ah_remove_from_cart",
		mcp.WithTitleAnnotation("Albert Heijn: Remove from Cart"),
		mcp.WithDescription(
			"Remove a single product from the Albert Heijn shopping cart. "+
				"Use product_id from ah_search_products or ah_get_cart.",
		),
		mcp.WithString("product_id",
			mcp.Required(),
			mcp.Description("Numeric product ID to remove"),
		),
	)
	s.AddTool(tool, withClient(deps, func(ctx context.Context, c *appie.Client, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		productID := req.GetInt("product_id", 0)
		if productID <= 0 {
			return errResult("product_id is required"), nil
		}

		if err := ensureActiveOrder(ctx, c, "ah_remove_from_cart"); err != nil {
			return errResult(fmt.Sprintf("Failed to get active order: %v", err)), nil
		}

		if err := withRetry(ctx, "ah_remove_from_cart", func() error {
			return c.RemoveFromOrder(ctx, productID)
		}); err != nil {
			return errResult(fmt.Sprintf("Failed to remove product %d from cart: %v", productID, err)), nil
		}
		return mcp.NewToolResultText(fmt.Sprintf("Product %d removed from cart.", productID)), nil
	}))
}

// --- ah_clear_cart ---

func registerClearCart(s *server.MCPServer, deps Deps) {
	tool := mcp.NewTool("ah_clear_cart",
		mcp.WithTitleAnnotation("Albert Heijn: Clear Cart"),
		mcp.WithDescription(
			"Remove ALL items from the Albert Heijn shopping cart. "+
				"Irreversible — requires confirm=\"yes\" to prevent accidental use.",
		),
		mcp.WithString("confirm",
			mcp.Required(),
			mcp.Description(`Must be "yes" to confirm clearing the entire cart`),
		),
	)
	s.AddTool(tool, withClient(deps, func(ctx context.Context, c *appie.Client, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if req.GetString("confirm", "") != "yes" {
			return errResult(`confirm must be "yes" to clear the cart`), nil
		}
		if err := withRetry(ctx, "ah_clear_cart", func() error {
			return c.ClearOrder(ctx)
		}); err != nil {
			return errResult(fmt.Sprintf("Failed to clear cart: %v", err)), nil
		}
		LogInfo("ah_clear_cart", "cart cleared")
		return mcp.NewToolResultText("Shopping cart cleared."), nil
	}))
}

// --- ah_reopen_order ---

func registerReopenOrder(s *server.MCPServer, deps Deps) {
	tool := mcp.NewTool("ah_reopen_order",
		mcp.WithTitleAnnotation("Albert Heijn: Reopen Order for Editing"),
		mcp.WithDescription(
			"Unlock a submitted AH delivery order so its items can be changed. "+
				"The order becomes the active order again (REOPENED state). "+
				"IMPORTANT: You MUST call ah_revert_order when done, even if editing fails — "+
				"otherwise the order stays unlocked and interferes with new orders. "+
				"Only works before the order's closing time; returns an error if too late. "+
				"Get order_id from ah_get_order_history (check modifiable=true first).",
		),
		mcp.WithString("order_id",
			mcp.Required(),
			mcp.Description("Numeric order ID from ah_get_order_history"),
		),
	)
	s.AddTool(tool, withClient(deps, func(ctx context.Context, c *appie.Client, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		orderID := req.GetInt("order_id", 0)
		if orderID <= 0 {
			return errResult("order_id is required"), nil
		}

		if err := withRetry(ctx, "ah_reopen_order", func() error {
			return c.ReopenOrder(ctx, orderID)
		}); err != nil {
			return errResult(fmt.Sprintf("Failed to reopen order %d: %v", orderID, err)), nil
		}
		LogInfo("ah_reopen_order", "order %d reopened", orderID)
		return mcp.NewToolResultText(fmt.Sprintf(
			"Order %d is now unlocked (REOPENED). Use ah_update_order_items to make changes, then call ah_revert_order when done.",
			orderID,
		)), nil
	}))
}

// --- ah_update_order_items ---

func registerUpdateOrderItems(s *server.MCPServer, deps Deps) {
	tool := mcp.NewTool("ah_update_order_items",
		mcp.WithTitleAnnotation("Albert Heijn: Update Order Items"),
		mcp.WithDescription(
			"Add, update, or remove items from the currently active AH order. "+
				"Set quantity=0 to remove an item. "+
				"Use after ah_reopen_order to edit a submitted delivery order. "+
				"Returns confirmation of the update.",
		),
		mcp.WithArray("items",
			mcp.Required(),
			mcp.Description(`Items to add/update/remove. Each: {"product_id": 123456, "quantity": 2} — set quantity=0 to remove.`),
			mcp.Items(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"product_id": map[string]any{"type": "integer"},
					"quantity":   map[string]any{"type": "integer"},
				},
				"required": []string{"product_id", "quantity"},
			}),
		),
	)
	s.AddTool(tool, withClient(deps, func(ctx context.Context, c *appie.Client, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// defaultQty 0: here a zero quantity is meaningful — it removes an item.
		items, err := parseLineItems(req.GetArguments()["items"], "items", 0)
		if err != nil {
			return errResult(err.Error()), nil
		}
		if len(items) == 0 {
			return errResult("no valid items provided"), nil
		}

		orderItems := make([]appie.OrderItem, 0, len(items))
		for _, it := range items {
			orderItems = append(orderItems, appie.OrderItem{ProductID: it.ProductID, Quantity: it.Quantity})
		}

		if err := ensureActiveOrder(ctx, c, "ah_update_order_items"); err != nil {
			return errResult(fmt.Sprintf("Failed to get active order: %v", err)), nil
		}

		if err := withRetry(ctx, "ah_update_order_items", func() error {
			return c.AddToOrder(ctx, orderItems)
		}); err != nil {
			return errResult(fmt.Sprintf("Failed to update order items: %v", err)), nil
		}

		added, removed := 0, 0
		for _, it := range orderItems {
			if it.Quantity == 0 {
				removed++
			} else {
				added++
			}
		}
		return mcp.NewToolResultText(fmt.Sprintf(
			"Order updated: %d item(s) added/changed, %d item(s) removed. Call ah_revert_order to resubmit.",
			added, removed,
		)), nil
	}))
}

// --- ah_revert_order ---

func registerRevertOrder(s *server.MCPServer, deps Deps) {
	tool := mcp.NewTool("ah_revert_order",
		mcp.WithTitleAnnotation("Albert Heijn: Resubmit Order"),
		mcp.WithDescription(
			"Resubmit a reopened AH delivery order back to its scheduled/submitted state. "+
				"ALWAYS call this after ah_reopen_order, whether editing succeeded or failed. "+
				"Clears the active order state on the client so the shopping cart works normally again.",
		),
		mcp.WithString("order_id",
			mcp.Required(),
			mcp.Description("Numeric order ID — same value passed to ah_reopen_order"),
		),
	)
	s.AddTool(tool, withClient(deps, func(ctx context.Context, c *appie.Client, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		orderID := req.GetInt("order_id", 0)
		if orderID <= 0 {
			return errResult("order_id is required"), nil
		}

		if err := withRetry(ctx, "ah_revert_order", func() error {
			return c.RevertOrder(ctx, orderID)
		}); err != nil {
			return errResult(fmt.Sprintf("Failed to revert order %d: %v", orderID, err)), nil
		}
		LogInfo("ah_revert_order", "order %d resubmitted", orderID)
		return mcp.NewToolResultText(fmt.Sprintf(
			"Order %d has been resubmitted. Your delivery is back on schedule.",
			orderID,
		)), nil
	}))
}
