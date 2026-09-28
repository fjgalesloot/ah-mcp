package tools

import (
	"context"
	"fmt"
	"strings"

	appie "github.com/gwillem/appie-go"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// shoppingListPath is the v2 Boodschappenlijst endpoint. The v3 /lists
// endpoint returns "Mijn Lijstjes" (saved favourite lists), which is a
// separate feature in the AH app and unrelated to the main list.
const shoppingListPath = "/mobile-services/shoppinglist/v2/items"

// RegisterBasketTools registers shopping-list MCP tools.
func RegisterBasketTools(s *server.MCPServer, deps Deps) {
	registerGetShoppingList(s, deps)
	registerAddToShoppingList(s, deps)
	registerAddFreeTextToShoppingList(s, deps)
	registerRemoveFromShoppingList(s, deps)
	registerCheckShoppingListItem(s, deps)
	registerClearShoppingList(s, deps)
	registerShoppingListToOrder(s, deps)
	registerGetFavoriteLists(s, deps)
	registerAddToFavoriteList(s, deps)
	registerRemoveFromFavoriteList(s, deps)
}

// v2ListItem mirrors an item as returned by the v2 Boodschappenlijst endpoint.
type v2ListItem struct {
	ListItemID     int    `json:"listItemId"`
	Position       int    `json:"position"`
	OriginCode     string `json:"originCode"`
	Quantity       int    `json:"quantity"`
	Type           string `json:"type"`
	Description    string `json:"description"`
	StrikedThrough bool   `json:"strikedthrough"`
	ProductDetails struct {
		Product struct {
			WebshopID int    `json:"webshopId"`
			Title     string `json:"title"`
		} `json:"product"`
	} `json:"productDetails"`
}

type v2ListResponse struct {
	Items []v2ListItem `json:"items"`
}

// name is the item's display name. AH leaves description empty for items added
// as products, where the title carries the name instead.
func (it v2ListItem) name() string {
	if it.Description != "" {
		return it.Description
	}
	return it.ProductDetails.Product.Title
}

func (it v2ListItem) productID() int {
	return it.ProductDetails.Product.WebshopID
}

// v2PatchItem is the write shape for the v2 list; quantity 0 deletes the item.
// AH rejects the PATCH when description, type or originCode are blank, so each
// falls back to the value the app would have sent.
type v2PatchItem struct {
	ProductID     int    `json:"productId,omitempty"`
	Description   string `json:"description"`
	Quantity      int    `json:"quantity"`
	Type          string `json:"type"`
	OriginCode    string `json:"originCode"`
	SearchTerm    string `json:"searchTerm,omitempty"`
	StrikeThrough bool   `json:"strikeThrough"`
}

// removalPatch builds the quantity-0 payload that deletes an item.
func removalPatch(it v2ListItem) v2PatchItem {
	return itemPatch(it, 0)
}

// itemPatch builds the payload that sets an existing item's quantity. The v2
// PATCH sets the quantity rather than adding to it, and unchecks the item.
func itemPatch(it v2ListItem, quantity int) v2PatchItem {
	name := it.name()
	itemType := it.Type
	if itemType == "" {
		itemType = "SHOPPABLE"
	}
	originCode := it.OriginCode
	if originCode == "" {
		originCode = "PRD"
	}
	return v2PatchItem{
		ProductID:     it.productID(),
		Description:   name,
		Quantity:      quantity,
		Type:          itemType,
		OriginCode:    originCode,
		SearchTerm:    name,
		StrikeThrough: false,
	}
}

// addPatches turns items to add into v2 PATCH payloads. Because the PATCH sets
// quantities, a product already on the list gets its current quantity plus
// the requested one — otherwise "add one more" would leave it unchanged. A
// checked-off item starts again from the requested quantity. Duplicate
// product IDs in items are summed. The payloads come out in the order the
// products first appear in items.
func addPatches(list []v2ListItem, items []lineItem) []v2PatchItem {
	existing := make(map[int]v2ListItem, len(list))
	for _, it := range list {
		if pid := it.productID(); pid > 0 {
			if _, seen := existing[pid]; !seen {
				existing[pid] = it
			}
		}
	}

	var order []int
	want := make(map[int]int, len(items))
	for _, it := range items {
		if _, seen := want[it.ProductID]; !seen {
			order = append(order, it.ProductID)
		}
		want[it.ProductID] += it.Quantity
	}

	out := make([]v2PatchItem, 0, len(order))
	for _, pid := range order {
		qty := want[pid]
		if cur, ok := existing[pid]; ok {
			if !cur.StrikedThrough {
				qty += cur.Quantity
			}
			out = append(out, itemPatch(cur, qty))
			continue
		}
		// Same shape appie-go's AddToShoppingList sends for a new product.
		out = append(out, v2PatchItem{
			ProductID:  pid,
			Quantity:   qty,
			Type:       "SHOPPABLE",
			OriginCode: "PRD",
		})
	}
	return out
}

// fetchShoppingList reads the current Boodschappenlijst.
func fetchShoppingList(ctx context.Context, c *appie.Client, tool string) (*v2ListResponse, error) {
	var list v2ListResponse
	if err := withRetry(ctx, tool, func() error {
		return c.DoRequest(ctx, "GET", shoppingListPath, nil, &list)
	}); err != nil {
		return nil, err
	}
	return &list, nil
}

// patchShoppingList applies a set of item mutations to the Boodschappenlijst.
func patchShoppingList(ctx context.Context, c *appie.Client, tool string, items []v2PatchItem) error {
	return withRetry(ctx, tool, func() error {
		return c.DoRequest(ctx, "PATCH", shoppingListPath, map[string]any{"items": items}, nil)
	})
}

// --- ah_get_shopping_list ---

func registerGetShoppingList(s *server.MCPServer, deps Deps) {
	tool := mcp.NewTool("ah_get_shopping_list",
		mcp.WithTitleAnnotation("Albert Heijn: View Shopping List"),
		mcp.WithDescription("Get the contents of your Albert Heijn shopping list. Returns item names, quantities, and product IDs."),
	)
	s.AddTool(tool, withClient(deps, func(ctx context.Context, c *appie.Client, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		list, err := fetchShoppingList(ctx, c, "ah_get_shopping_list")
		if err != nil {
			return errResult(fmt.Sprintf("Failed to get shopping list: %v", err)), nil
		}
		if len(list.Items) == 0 {
			return mcp.NewToolResultText("Your shopping list is empty."), nil
		}

		type entry struct {
			Position  int    `json:"position"`
			ItemID    int    `json:"item_id,omitempty"`
			Name      string `json:"name"`
			ProductID int    `json:"product_id,omitempty"`
			Quantity  int    `json:"quantity"`
			Checked   bool   `json:"checked,omitempty"`
		}
		entries := make([]entry, 0, len(list.Items))
		for _, item := range list.Items {
			entries = append(entries, entry{
				Position:  item.Position,
				ItemID:    item.ListItemID,
				Name:      item.name(),
				ProductID: item.productID(),
				Quantity:  item.Quantity,
				Checked:   item.StrikedThrough,
			})
		}
		return jsonResult(entries)
	}))
}

// --- ah_add_to_shopping_list ---

func registerAddToShoppingList(s *server.MCPServer, deps Deps) {
	tool := mcp.NewTool("ah_add_to_shopping_list",
		mcp.WithTitleAnnotation("Albert Heijn: Add to Shopping List"),
		mcp.WithDescription(
			"Add one or more products to your Albert Heijn shopping list. "+
				"Pass an array of items, each with product_id (int) and quantity (int). "+
				"Quantities are added to what is already on the list: adding 1 of a product that is listed twice leaves 3. "+
				"Returns confirmation listing each product's new quantity on the list.",
		),
		mcp.WithString("items",
			mcp.Required(),
			mcp.Description(`JSON array of items to add. Each item needs product_id (int) and quantity (int). Example: [{"product_id": 123456, "quantity": 2}]`),
		),
	)
	s.AddTool(tool, withClient(deps, func(ctx context.Context, c *appie.Client, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		items, err := parseLineItems(req.GetArguments()["items"], "items", 1)
		if err != nil {
			return errResult(err.Error()), nil
		}
		if len(items) == 0 {
			return errResult("no valid items provided (each item needs product_id > 0 and quantity > 0)"), nil
		}

		// The PATCH sets quantities, so read the list first and send totals.
		// Totals also make the retried PATCH idempotent.
		list, err := fetchShoppingList(ctx, c, "ah_add_to_shopping_list")
		if err != nil {
			return errResult(fmt.Sprintf("Failed to read shopping list: %v", err)), nil
		}
		patches := addPatches(list.Items, items)
		if err := patchShoppingList(ctx, c, "ah_add_to_shopping_list", patches); err != nil {
			return errResult(fmt.Sprintf("Failed to add items: %v", err)), nil
		}

		// Fetch product names for the confirmation message.
		pids := make([]int, 0, len(patches))
		for _, p := range patches {
			pids = append(pids, p.ProductID)
		}
		nameMap := map[int]string{}
		if products, pErr := c.GetProductsByIDs(ctx, pids); pErr == nil {
			for _, p := range products {
				nameMap[p.ID] = p.Title
			}
		}

		names := make([]string, 0, len(patches))
		for _, p := range patches {
			name, ok := nameMap[p.ProductID]
			if !ok {
				name = fmt.Sprintf("Product %d", p.ProductID)
			}
			names = append(names, fmt.Sprintf("%s (now x%d)", name, p.Quantity))
		}
		return mcp.NewToolResultText(fmt.Sprintf("Added to shopping list:\n- %s", strings.Join(names, "\n- "))), nil
	}))
}

// --- ah_add_free_text_to_shopping_list ---

func registerAddFreeTextToShoppingList(s *server.MCPServer, deps Deps) {
	tool := mcp.NewTool("ah_add_free_text_to_shopping_list",
		mcp.WithTitleAnnotation("Albert Heijn: Add Free-Text to Shopping List"),
		mcp.WithDescription(
			"Add a free-text item to the Albert Heijn shopping list (no product ID needed). "+
				"Use for reminders like 'verse bloemen', 'any good wine', or items not found in search.",
		),
		mcp.WithString("name",
			mcp.Required(),
			mcp.Description("Free-text item description, e.g. 'verse bloemen', 'goede rode wijn'"),
		),
		mcp.WithString("quantity",
			mcp.Description("Quantity (default 1)"),
		),
	)
	s.AddTool(tool, withClient(deps, func(ctx context.Context, c *appie.Client, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		name := strings.TrimSpace(req.GetString("name", ""))
		if name == "" {
			return errResult("name is required"), nil
		}
		quantity := req.GetInt("quantity", 1)
		if quantity < 1 {
			quantity = 1
		}

		if err := withRetry(ctx, "ah_add_free_text_to_shopping_list", func() error {
			return c.AddFreeTextToShoppingList(ctx, name, quantity)
		}); err != nil {
			return errResult(fmt.Sprintf("Failed to add free-text item: %v", err)), nil
		}
		return mcp.NewToolResultText(fmt.Sprintf("Added '%s' (x%d) to shopping list.", name, quantity)), nil
	}))
}

// --- ah_remove_from_shopping_list ---

func registerRemoveFromShoppingList(s *server.MCPServer, deps Deps) {
	tool := mcp.NewTool("ah_remove_from_shopping_list",
		mcp.WithTitleAnnotation("Albert Heijn: Remove from Shopping List"),
		mcp.WithDescription(
			"Remove one or more items from the Albert Heijn Boodschappenlijst. "+
				"For product items pass product_ids; for free-text items pass names. "+
				"Get product_ids from ah_get_shopping_list.",
		),
		mcp.WithString("product_ids",
			mcp.Description("JSON array of product IDs to remove, e.g. [123456, 789012]. Use for product items."),
		),
		mcp.WithString("names",
			mcp.Description(`JSON array of free-text item names to remove, e.g. ["verse bloemen"]. Use for items without a product ID.`),
		),
	)
	s.AddTool(tool, withClient(deps, func(ctx context.Context, c *appie.Client, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		productIDs, err := parseIntArray(req.GetArguments()["product_ids"], "product_ids")
		if err != nil {
			return errResult(err.Error()), nil
		}
		names, err := parseStringArray(req.GetArguments()["names"], "names")
		if err != nil {
			return errResult(err.Error()), nil
		}
		if len(productIDs) == 0 && len(names) == 0 {
			return errResult("provide at least one product_id or name to remove"), nil
		}

		list, err := fetchShoppingList(ctx, c, "ah_remove_from_shopping_list")
		if err != nil {
			return errResult(fmt.Sprintf("Failed to read shopping list: %v", err)), nil
		}

		// Build set of things to remove. parseStringArray drops blank names, so
		// an empty entry cannot match every unlabelled item on the list.
		removeByProductID := make(map[int]bool, len(productIDs))
		for _, pid := range productIDs {
			removeByProductID[pid] = true
		}
		removeByName := make(map[string]bool, len(names))
		for _, n := range names {
			removeByName[strings.ToLower(strings.TrimSpace(n))] = true
		}

		var (
			removeItems []v2PatchItem
			removed     []string
		)
		for _, it := range list.Items {
			pid := it.productID()
			matchesID := pid > 0 && removeByProductID[pid]
			matchesName := removeByName[strings.ToLower(it.name())]
			if !matchesID && !matchesName {
				continue
			}
			removeItems = append(removeItems, removalPatch(it))
			removed = append(removed, it.name())
		}
		if len(removeItems) == 0 {
			return errResult("no matching items found on the shopping list"), nil
		}

		if err := patchShoppingList(ctx, c, "ah_remove_from_shopping_list", removeItems); err != nil {
			return errResult(fmt.Sprintf("Failed to update shopping list: %v", err)), nil
		}
		LogInfo("ah_remove_from_shopping_list", "removed=%d", len(removeItems))
		return mcp.NewToolResultText(fmt.Sprintf("Removed from shopping list: %s", strings.Join(removed, ", "))), nil
	}))
}

// --- ah_check_shopping_list_item ---

func registerCheckShoppingListItem(s *server.MCPServer, deps Deps) {
	tool := mcp.NewTool("ah_check_shopping_list_item",
		mcp.WithTitleAnnotation("Albert Heijn: Check Shopping List Item"),
		mcp.WithDescription(
			"Mark a favourite-list item as checked (picked up) or uncheck it. "+
				"Only works for items in 'Mijn Lijstjes' (favourite lists), which have real string item IDs. "+
				"The main Boodschappenlijst returns listItemId=0 for every item, so items from "+
				"ah_get_shopping_list cannot be checked here — do that in the AH app.",
		),
		mcp.WithString("item_id",
			mcp.Required(),
			mcp.Description("Item ID (string) from a favourite list — NOT from ah_get_shopping_list (those have no usable IDs)"),
		),
		mcp.WithString("checked",
			mcp.Description("'true' to check the item (default), 'false' to uncheck"),
		),
	)
	s.AddTool(tool, withClient(deps, func(ctx context.Context, c *appie.Client, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		itemID := strings.TrimSpace(req.GetString("item_id", ""))
		if itemID == "" {
			return errResult("item_id is required"), nil
		}
		if itemID == "0" {
			return errResult("item_id 0 comes from the main Boodschappenlijst, which does not support checking items. Use a favourite-list item ID from ah_get_favorite_lists."), nil
		}
		checked := req.GetString("checked", "true") != "false"

		if err := withRetry(ctx, "ah_check_shopping_list_item", func() error {
			return c.CheckShoppingListItem(ctx, itemID, checked)
		}); err != nil {
			return errResult(fmt.Sprintf("Failed to update item %s: %v", itemID, err)), nil
		}
		state := "checked"
		if !checked {
			state = "unchecked"
		}
		return mcp.NewToolResultText(fmt.Sprintf("Item %s marked as %s.", itemID, state)), nil
	}))
}

// --- ah_clear_shopping_list ---

func registerClearShoppingList(s *server.MCPServer, deps Deps) {
	tool := mcp.NewTool("ah_clear_shopping_list",
		mcp.WithTitleAnnotation("Albert Heijn: Clear Shopping List"),
		mcp.WithDescription(
			"Remove ALL items from the Albert Heijn shopping list. "+
				"Irreversible — requires confirm=\"yes\".",
		),
		mcp.WithString("confirm",
			mcp.Required(),
			mcp.Description("Must be \"yes\" to confirm clearing the list"),
		),
	)
	s.AddTool(tool, withClient(deps, func(ctx context.Context, c *appie.Client, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if req.GetString("confirm", "") != "yes" {
			return errResult("confirm must be \"yes\" to clear the shopping list"), nil
		}

		list, err := fetchShoppingList(ctx, c, "ah_clear_shopping_list")
		if err != nil {
			return errResult(fmt.Sprintf("Failed to read shopping list: %v", err)), nil
		}
		if len(list.Items) == 0 {
			return mcp.NewToolResultText("Shopping list is already empty."), nil
		}

		zeros := make([]v2PatchItem, 0, len(list.Items))
		for _, it := range list.Items {
			zeros = append(zeros, removalPatch(it))
		}
		if err := patchShoppingList(ctx, c, "ah_clear_shopping_list", zeros); err != nil {
			return errResult(fmt.Sprintf("Failed to clear shopping list: %v", err)), nil
		}
		LogInfo("ah_clear_shopping_list", "cleared=%d", len(zeros))
		return mcp.NewToolResultText("Shopping list cleared."), nil
	}))
}

// --- ah_shopping_list_to_order ---

func registerShoppingListToOrder(s *server.MCPServer, deps Deps) {
	tool := mcp.NewTool("ah_shopping_list_to_order",
		mcp.WithTitleAnnotation("Albert Heijn: Move List to Cart"),
		mcp.WithDescription(
			"Add all unchecked product items from the Albert Heijn shopping list to the online order (cart). "+
				"Free-text items and already-checked items are skipped. "+
				"Use ah_get_cart to review the result.",
		),
	)
	s.AddTool(tool, withClient(deps, func(ctx context.Context, c *appie.Client, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if err := withRetry(ctx, "ah_shopping_list_to_order", func() error {
			return c.ShoppingListToOrder(ctx)
		}); err != nil {
			return errResult(fmt.Sprintf("Failed to move shopping list to order: %v", err)), nil
		}
		return mcp.NewToolResultText("Shopping list items added to your online order. Use ah_get_cart to review."), nil
	}))
}

// --- ah_get_favorite_lists ---

func registerGetFavoriteLists(s *server.MCPServer, deps Deps) {
	tool := mcp.NewTool("ah_get_favorite_lists",
		mcp.WithTitleAnnotation("Albert Heijn: View Favourite Lists"),
		mcp.WithDescription(
			"List all Albert Heijn favorite/saved shopping lists with their names and item counts. "+
				"Use the returned list ID with ah_add_to_favorite_list or ah_remove_from_favorite_list.",
		),
	)
	s.AddTool(tool, withClient(deps, func(ctx context.Context, c *appie.Client, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var lists []appie.ShoppingList
		if err := withRetry(ctx, "ah_get_favorite_lists", func() error {
			var e error
			lists, e = c.GetShoppingLists(ctx, 0)
			return e
		}); err != nil {
			return errResult(fmt.Sprintf("Failed to get favorite lists: %v", err)), nil
		}

		type entry struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			ItemCount int    `json:"item_count"`
		}
		results := make([]entry, 0, len(lists))
		for _, l := range lists {
			results = append(results, entry{ID: l.ID, Name: l.Name, ItemCount: l.ItemCount})
		}
		return jsonResult(results)
	}))
}

// --- ah_add_to_favorite_list ---

func registerAddToFavoriteList(s *server.MCPServer, deps Deps) {
	tool := mcp.NewTool("ah_add_to_favorite_list",
		mcp.WithTitleAnnotation("Albert Heijn: Add to Favourite List"),
		mcp.WithDescription(
			"Add products to a named Albert Heijn favorite list. "+
				"Get list_id from ah_get_favorite_lists.",
		),
		mcp.WithString("list_id",
			mcp.Required(),
			mcp.Description("Favorite list ID from ah_get_favorite_lists"),
		),
		mcp.WithString("items",
			mcp.Required(),
			mcp.Description("JSON array of items: [{\"product_id\": 123456, \"quantity\": 1}]"),
		),
	)
	s.AddTool(tool, withClient(deps, func(ctx context.Context, c *appie.Client, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		listID := strings.TrimSpace(req.GetString("list_id", ""))
		if listID == "" {
			return errResult("list_id is required"), nil
		}
		items, err := parseLineItems(req.GetArguments()["items"], "items", 1)
		if err != nil {
			return errResult(err.Error()), nil
		}
		if len(items) == 0 {
			return errResult("no valid items provided"), nil
		}

		listItems := toListItems(items)
		if err := withRetry(ctx, "ah_add_to_favorite_list", func() error {
			return c.AddToFavoriteList(ctx, listID, listItems)
		}); err != nil {
			return errResult(fmt.Sprintf("Failed to add to favorite list: %v", err)), nil
		}
		return mcp.NewToolResultText(fmt.Sprintf("Added %d item(s) to favorite list %s.", len(listItems), listID)), nil
	}))
}

// --- ah_remove_from_favorite_list ---

func registerRemoveFromFavoriteList(s *server.MCPServer, deps Deps) {
	tool := mcp.NewTool("ah_remove_from_favorite_list",
		mcp.WithTitleAnnotation("Albert Heijn: Remove from Favourite List"),
		mcp.WithDescription(
			"Remove products from a named Albert Heijn favorite list. "+
				"Get list_id from ah_get_favorite_lists.",
		),
		mcp.WithString("list_id",
			mcp.Required(),
			mcp.Description("Favorite list ID from ah_get_favorite_lists"),
		),
		mcp.WithString("product_ids",
			mcp.Required(),
			mcp.Description("JSON array of product IDs to remove, e.g. [123456, 789012]"),
		),
	)
	s.AddTool(tool, withClient(deps, func(ctx context.Context, c *appie.Client, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		listID := strings.TrimSpace(req.GetString("list_id", ""))
		if listID == "" {
			return errResult("list_id is required"), nil
		}
		productIDs, err := parseIntArray(req.GetArguments()["product_ids"], "product_ids")
		if err != nil {
			return errResult(err.Error()), nil
		}
		if len(productIDs) == 0 {
			return errResult("no valid product_ids provided"), nil
		}

		if err := withRetry(ctx, "ah_remove_from_favorite_list", func() error {
			return c.RemoveFromFavoriteList(ctx, listID, productIDs)
		}); err != nil {
			return errResult(fmt.Sprintf("Failed to remove from favorite list: %v", err)), nil
		}
		return mcp.NewToolResultText(fmt.Sprintf("Removed %d product(s) from favorite list %s.", len(productIDs), listID)), nil
	}))
}
