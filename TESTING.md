# ah-mcp Manual Testing Scenarios

Covers all 28 MCP tools in a logical order. Run these from any MCP client (Claude Desktop recommended).
Each scenario shows the prompt to use and what a passing result looks like.

> **Testing status (v1.0.0):** Tested with **Claude Desktop** in **stdio mode** only.
> SSE transport, Claude.ai web, Windsurf, Cursor, and ChatGPT Desktop are listed in the README but untested.
> Items marked ⚠️ are known to be broken or unconfirmed.

---

## 1. Authentication

### 1.1 Login (first time)
```
Prompt: "Log in to Albert Heijn"
Tool:   ah_login (call 1)
Expect: A URL starting with http://127.0.0.1:9876/<secret>/login?...
Action: Open the URL in your browser and complete the AH login.
```

### 1.2 Confirm login
```
Prompt: "Check if I'm logged in to AH"
Tool:   ah_login (call 2)
Expect: "Login successful! Connected as <First> <Last>."
```

### 1.3 Already logged in
```
Prompt: "Log in to Albert Heijn"
Tool:   ah_login
Expect: "Already connected as <First> <Last>." (no new URL)
```

---

## 2. Member profile

### 2.1 Fetch profile
```
Prompt: "Show my AH member profile"
Tool:   ah_get_member_profile
Expect: JSON with name, email, bonus_card_number (masked: ****1234)
```

---

## 3. Product search

### 3.1 Basic Dutch search
```
Prompt: "Search for melk in AH"
Tool:   ah_search_products  query=melk
Expect: List of milk products with id, title, price, is_bonus
```

### 3.2 English search
```
Prompt: "Search for chicken in AH, show 5 results"
Tool:   ah_search_products  query=chicken  limit=5
Expect: Up to 5 chicken products
```

### 3.3 Filtered search — bonus only
```
Prompt: "Search for kaas that is currently on promotion"
Tool:   ah_search_products_filtered  query=kaas  bonus=true
Expect: Only products where is_bonus=true
```

### 3.4 Single product detail
```
Prompt: "Give me full details for product <id from 3.1>"
Tool:   ah_get_product  product_id=<id>
Expect: Full product with brand, category, nutri_score, unit_size, image_url
```

### 3.5 Product with nutritional info
```
Prompt: "Show me the nutritional values for product <id>"
Tool:   ah_get_product  product_id=<id>  include_nutritional_info=true
Expect: Same as 3.4 plus nutritional_info array (calories, fat, protein, etc.)
```

---

## 4. Bonus offers

### 4.1 All current bonuses
```
Prompt: "What is currently on bonus at AH?"
Tool:   ah_get_bonus_offers
Expect: List of products with original_price, bonus_price, discount_percentage, bonus_mechanism
```

### 4.2 Filtered bonus search
```
Prompt: "What yoghurt is on bonus?"
Tool:   ah_get_bonus_offers  query=yoghurt
Expect: Bonus products whose title contains "yoghurt"
```

### 4.3 Drill into a bonus group
```
Prereq: Note a bonus_segment_id from step 4.1 (only present on group-type promotions)
Prompt: "Show me all products in that bonus deal group"
Tool:   ah_get_bonus_group_products  segment_id=<bonus_segment_id>
Expect: All qualifying products for that specific promotion
```

---

## 5. Stores & last-chance items

### 5.1 Find nearby stores
```
Prompt: "Find AH stores near me" (or "near postal code 1011AB")
Tool:   ah_search_stores
Expect: List of stores with id, name, type, street, city
        Note a store_id for step 5.2
```

### 5.2 Last-chance bargains
```
Prompt: "Show me vandaag-af items at store <id>"
Tool:   ah_get_last_chance_items  store_id=<id>
Expect: List with title, price_now, price_was, discount_percentage, expiration_date
```

### 5.3 Last-chance auto-resolve store
```
Prompt: "Show me last-chance items near postal code 1234AB"
Tool:   ah_get_last_chance_items  postal_code=1234AB
Expect: Bargains from the nearest store to that postal code
```

---

## 6. Shopping list

### 6.1 View shopping list
```
Prompt: "Show my AH shopping list"
Tool:   ah_get_shopping_list
Expect: JSON array with id, name, product_id, quantity, checked
        Note an item_id for step 6.4
```

### 6.2 Add product items
```
Prereq: product_id from step 3.1
Prompt: "Add 2 of product <id> to my AH shopping list"
Tool:   ah_add_to_shopping_list  items=[{"product_id":<id>,"quantity":2}]
Expect: "Added to shopping list: <Product Name> (x2)"
```

### 6.3 Add free-text item
```
Prompt: "Add 'verse bloemen' to my shopping list"
Tool:   ah_add_free_text_to_shopping_list  name=verse bloemen  quantity=1
Expect: "Added 'verse bloemen' (x1) to shopping list."
```

### 6.4 Remove an item from shopping list
```
Prereq: product_id from step 6.2
Prompt: "Remove product <id> from my AH shopping list"
Tool:   ah_remove_from_shopping_list  product_ids=[<id>]
Expect: "Removed from shopping list: product <id>"
Verify: ah_get_shopping_list no longer contains the item
```

### 6.5 Remove a free-text item
```
Prereq: free-text item added in step 6.3
Prompt: "Remove 'verse bloemen' from my shopping list"
Tool:   ah_remove_from_shopping_list  names=["verse bloemen"]
Expect: "Removed from shopping list: verse bloemen"
```

### 6.6 Check an item ⚠️ NOT WORKING
```
⚠️ KNOWN BROKEN: The AH v2 Boodschappenlijst API returns listItemId=0 for all items,
   so per-item check/uncheck is impossible via this endpoint.
   This can only be done in the AH app directly.

Prereq: item_id from step 6.1
Prompt: "Mark item <item_id> as checked on my shopping list"
Tool:   ah_check_shopping_list_item  item_id=<id>  checked=true
Expect: "Item <id> marked as checked."  ← will fail (no real item IDs)
```

### 6.7 Uncheck an item ⚠️ NOT WORKING
```
⚠️ Same limitation as 6.6 — listItemId is always 0.
```

### 6.8 View favourite lists
```
Prompt: "Show all my AH favourite lists"
Tool:   ah_get_favorite_lists
Expect: Array with id, name, item_count
        Note a list_id for steps 6.9–6.10
```

### 6.9 Add to favourite list
```
Prereq: list_id from 6.8, product_id from 3.1
Prompt: "Add product <id> to my favourite list <list_id>"
Tool:   ah_add_to_favorite_list  list_id=<id>  items=[{"product_id":<id>,"quantity":1}]
Expect: "Added 1 item(s) to favorite list <list_id>."
```

### 6.10 Remove from favourite list
```
Prompt: "Remove product <id> from favourite list <list_id>"
Tool:   ah_remove_from_favorite_list  list_id=<id>  product_ids=[<product_id>]
Expect: "Removed 1 product(s) from favorite list <list_id>."
```

### 6.11 Clear shopping list
```
Prompt: "Clear my AH shopping list"
Tool:   ah_clear_shopping_list  confirm=yes
Expect: "Shopping list cleared."
Verify: Run ah_get_shopping_list — should return empty array
```

---

## 7. Shopping cart

### 7.1 Add something to search for first
```
Prompt: "Search for appelsap in AH"
Tool:   ah_search_products  query=appelsap
Note:   Save a product_id for cart tests
```

### 7.2 View cart
```
Prompt: "Show my AH shopping cart"
Tool:   ah_get_cart
Expect: JSON with id, state, items[], total_price, total_discount
```

### 7.3 Cart summary
```
Prompt: "What is the total of my AH cart?"
Tool:   ah_get_cart_summary
Expect: JSON with TotalItems, TotalPrice, TotalDiscount, DeliveryCost
```

### 7.4 Update cart item
```
Prompt: "Add 3 of product <id> to my AH cart"
Tool:   ah_update_cart_item  product_id=<id>  quantity=3
Expect: "Product <id> quantity set to 3."
Verify: ah_get_cart shows the item with quantity=3
```

### 7.5 Update quantity down
```
Prompt: "Change product <id> in my cart to quantity 1"
Tool:   ah_update_cart_item  product_id=<id>  quantity=1
Expect: "Product <id> quantity set to 1."
```

### 7.6 Remove from cart
```
Prompt: "Remove product <id> from my cart"
Tool:   ah_remove_from_cart  product_id=<id>
Expect: "Product <id> removed from cart."
Verify: ah_get_cart no longer contains the item
```

### 7.7 Shopping list to order
```
Prereq: Add some items back to shopping list (step 6.2)
Prompt: "Move my shopping list to my AH cart"
Tool:   ah_shopping_list_to_order
Expect: "Shopping list items added to your online order."
Verify: ah_get_cart shows the added items
```

### 7.8 Clear cart
```
Prompt: "Clear my AH shopping cart"
Tool:   ah_clear_cart  confirm=yes
Expect: "Shopping cart cleared."
Verify: ah_get_cart returns empty items array
```

---

## 8. Order history

### 8.1 List upcoming orders
```
Prompt: "Show my AH order history"
Tool:   ah_get_order_history
Expect: Array with id, date, time_window, total_price, status, modifiable
        Note an order_id for step 8.2
```

### 8.2 List past orders
```
Prompt: "Show my past AH orders"
Tool:   ah_get_past_orders  limit=10
Expect: Array with id, date, time_window, total_price, status
Note:   Uses CLOSED fulfillment status — returns empty if AH has no closed orders for account
```

### 8.3 Order details
```
Prompt: "Show me the items in order <order_id>"
Tool:   ah_get_order_details  order_id=<id>
Expect: Full item list with product names, quantities, prices
```

### 8.4 Frequent items
```
Prompt: "What do I order most often from AH?"
Tool:   ah_get_frequent_items  min_order_count=2
Expect: Products sorted by order_count descending
Note:   Scans both open and closed fulfillments; returns empty if no order history available
```

### 8.5 Edit a submitted order ⚠️ UNCONFIRMED
```
⚠️ UNCONFIRMED: The API calls succeed (reopen → add → revert) but changes did not
   appear on the AH website or in ah_get_order_details during testing. This may only
   work on orders in CONFIRMED state (not UNCONFIRMED). Needs further investigation.
   Do NOT rely on this for real order modifications until confirmed.

Prereq: An order from 8.1 where modifiable=true and state=CONFIRMED

Step A — Reopen:
Prompt: "Reopen order <order_id> so I can edit it"
Tool:   ah_reopen_order  order_id=<id>
Expect: "Order <id> is now unlocked (REOPENED)."

Step B — Update items:
Prompt: "Add product <id> quantity 2 to the reopened order"
Tool:   ah_update_order_items  items=[{"product_id":<id>,"quantity":2}]
Expect: "Order updated: 1 item(s) added/changed, 0 item(s) removed."

Step C — Revert (ALWAYS required, even if editing failed):
Prompt: "Resubmit order <order_id>"
Tool:   ah_revert_order  order_id=<id>
Expect: "Order <id> has been resubmitted. Your delivery is back on schedule."
```

---

## 9. Receipts

### 9.1 List receipts
```
Prompt: "Show my recent AH receipts"
Tool:   ah_get_receipts  limit=5
Expect: Array with id, date (YYYY-MM-DD HH:MM), total_amount
        Note a receipt_id for step 9.2
```

### 9.2 Receipt details
```
Prompt: "Show me what I bought in receipt <id>"
Tool:   ah_get_receipt_details  id=<receipt_id>
Expect: Full item list with name, quantity, unit_price, total
        Plus discounts[] and payments[] sections
```

---

## 10. Logout & re-login

### 10.1 Logout
```
Prompt: "Log out of Albert Heijn"
Tool:   ah_logout
Expect: "Logged out. Call ah_login to authenticate again."
Verify: Calling ah_get_member_profile returns "Not logged in" error
```

### 10.2 Re-login
```
Prompt: "Log in to Albert Heijn again"
Tool:   ah_login (two calls, same as section 1)
Expect: URL → browser login → "Login successful! Connected as <Name>."
```

---

## Coverage summary

> Tested: **Claude Desktop, stdio mode only.**

| Category | Tools | Status |
|---|---|---|
| Auth | ah_login, ah_logout | ✅ |
| Member | ah_get_member_profile | ✅ |
| Products | ah_search_products, ah_search_products_filtered, ah_get_product, ah_get_bonus_offers, ah_get_bonus_group_products | ✅ |
| Stores | ah_search_stores, ah_get_last_chance_items | ✅ |
| Shopping list | ah_get_shopping_list, ah_add_to_shopping_list, ah_add_free_text_to_shopping_list, ah_remove_from_shopping_list, ah_clear_shopping_list, ah_shopping_list_to_order, ah_get_favorite_lists, ah_add_to_favorite_list, ah_remove_from_favorite_list | ✅ |
| Shopping list | ah_check_shopping_list_item | ⚠️ broken (listItemId=0) |
| Cart | ah_get_cart, ah_get_cart_summary, ah_update_cart_item, ah_remove_from_cart, ah_clear_cart | ✅ |
| Order history | ah_get_order_history, ah_get_past_orders, ah_get_order_details, ah_get_frequent_items | ✅ |
| Order editing | ah_reopen_order, ah_update_order_items, ah_revert_order | ⚠️ unconfirmed |
| Receipts | ah_get_receipts, ah_get_receipt_details | ✅ |

**Total: 28 tools**
