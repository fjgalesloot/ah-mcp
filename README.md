# ah-mcp — Albert Heijn MCP Server

[![License: AGPL-3.0](https://img.shields.io/badge/License-AGPL--3.0-blue.svg)](LICENSE)

A [Model Context Protocol](https://modelcontextprotocol.io) server for the Albert Heijn supermarket API. Works with any MCP-compatible client.

## What you can do

Things the AH app and website can't do — but your AI assistant can:

**Smart shopping**
> *"Check my shopping list and add everything that's currently on bonus to my cart"*

> *"I want to make spaghetti bolognese for 4 people — find the ingredients at AH and add them to my list"*

> *"Find me a healthy snack that's on bonus and costs less than €2"*

**Know your habits**
> *"What do I order most often? Show me the top 10 and check which ones are on bonus this week"*

> *"What did I spend on groceries last month based on my kassabonnen?"*

> *"I usually buy melk, kaas, and brood — am I missing any of them in my current cart?"*

**Last-minute deals**
> *"What vandaag-af items are available near postal code 1234AB? Anything worth getting?"*

> *"Are there any bonus deals on dairy products this week?"*

**Cart management**
> *"Clear my cart and rebuild it from my shopping list"*

> *"I'm over budget — which items in my cart are not on bonus and could be swapped for cheaper alternatives?"*

---

## What this is

**ah-mcp** exposes the Albert Heijn mobile API as MCP tools so your AI assistant can:

- Search products, check bonus offers, and drill into promotion groups
- Browse last-chance / vandaag-af clearance items (store-specific)
- Manage your online shopping cart (view, add, update, remove, clear)
- Note: on the AH site, you may need to choose an order moment before the cart accepts new items
- View order history, order details, and frequently bought items
- Read and update your shopping list and named favourite lists
- Move your shopping list directly to your online order
- View in-store receipts (kassabonnen) with full item details
- Fetch your member profile and find nearby stores

Authentication is handled entirely through a reverse-proxy OAuth flow — no tokens are ever stored in the project directory.

## Compatibility

| Client | Transport | Status |
|---|---|---|
| Claude Desktop | stdio | ✅ tested |
| Claude Desktop | Streamable HTTP | ✅ tested |
| Claude.ai (web) | Streamable HTTP | ✅ tested |

## Quick start (pre-built binary)

1. Download the latest binary from Releases.
2. Run it:
   ```bash
   ./ah-mcp --transport stdio              # local client — browser opens automatically on login
   ./ah-mcp --transport sse                # local SSE on 127.0.0.1 — browser opens automatically
   ./ah-mcp --transport streamable-http    # local Streamable HTTP on 127.0.0.1

   # Remote: binds 0.0.0.0 and returns a login URL instead of opening a browser.
   # AH_MCP_TOKEN is required — startup fails without it.
   AH_MCP_TOKEN=$(openssl rand -hex 32) ./ah-mcp --transport streamable-http --remote
   ```
3. Ask your AI assistant to call **`ah_login`**. In local mode the browser opens automatically; in remote mode the assistant returns a URL for you to open.

## Build from source

```bash
git clone https://github.com/mrserzhan/ah-mcp
cd ah-mcp
go build -o ah-mcp .
```

Requires Go 1.23+.

## Environment variables

| Variable | Default | Description |
|---|---|---|
| `AH_SITE` | `nl` | Albert Heijn site to target. Supported values: `nl` and `be`. This switches the API/login hosts and the `X-Application` header used for requests. The Belgian `AHBEWEBSHOP` header mapping is based on reverse-engineering notes here: https://gist.github.com/jabbink/8bfa44bdfc535d696b340c46d228fdd1 |
| `AH_LOG_FILE` | unset | Optional log file path. When set, tool/auth logs are written to stderr and appended to this file. |
| `AH_CALLBACK_HOST` | `http://127.0.0.1:9876` | Base URL for the OAuth proxy. Users open this URL in their browser during login. Override to your server's public URL for remote deployments. Don't use `localhost`: hCaptcha refuses that hostname, so AH's login page can't load its captcha. |
| `AH_CALLBACK_PORT` | `9876` | Port the temporary OAuth reverse-proxy server listens on. |
| `AH_MCP_PORT` | `3000` | Port for the MCP HTTP server (`--transport sse` or `--transport streamable-http`). |
| `AH_MCP_BASE_URL` | `http://localhost:3000` | Public base URL advertised to MCP clients. **Must be set for remote deployments** — otherwise clients receive a `localhost` URL they cannot reach. Example: `https://myserver.example.com` |
| `AH_TOKENS_PATH` | `~/.config/ah-mcp/tokens.json` | Override the XDG token storage path. Directory is created automatically (mode `0700`). File is written with mode `0600`. |
| `AH_REMOTE` | `false` | Set to `true` to enable remote mode (same as `--remote` flag). Disables automatic browser opening on login. |
| `AH_MCP_TOKEN` | *(unset)* | Secret token required to access the HTTP server. Supply it as `Authorization: Bearer <token>` or `?token=<token>`. A non-loopback bind requires this or OAuth — startup fails with neither. Still accepted when OAuth is enabled. |
| `AH_MCP_OAUTH_ISSUER` | *(unset)* | Enables OAuth: the issuer URL of the authorization server, exactly as it appears in the tokens' `iss` claim. For Authentik: `https://authentik.example.com/application/o/<slug>/` (trailing slash included). Must be `https`. See [OAuth with Authentik](#oauth-with-authentik). |
| `AH_MCP_OAUTH_AUDIENCE` | *(unset)* | Required with `AH_MCP_OAUTH_ISSUER`: the `aud` value access tokens must carry. For Authentik this is the provider's client ID. |
| `AH_MCP_BIND` | loopback, or `0.0.0.0` with `--remote` | Interface to listen on (host only, no port). Set to `0.0.0.0` for containers that need a public bind without remote mode. Requires `AH_MCP_TOKEN` or OAuth. |
| `AH_MCP_ALLOWED_ORIGINS` | base URL + localhost + `https://claude.ai` | Comma-separated browser origins allowed to call the server. Requests with no `Origin` header (all non-browser MCP clients) always pass. Set to `*` to disable the check. |

Copy `.env.example` to `.env` and uncomment lines you want to change.

## Security

This server acts on a logged-in Albert Heijn account: anything that can reach it can read your orders, receipts, address and date of birth, and change your cart. The defaults are set accordingly.

- **Binds loopback by default.** `sse` and `streamable-http` listen on `127.0.0.1` unless you pass `--remote` (or set `AH_REMOTE=true` / `AH_MCP_BIND`).
- **A network bind requires auth.** Starting with a non-loopback address and neither `AH_MCP_TOKEN` nor OAuth is refused outright. A loopback server without either logs a warning — any local process can then use your session.
- **OAuth tokens are fully validated.** With `AH_MCP_OAUTH_ISSUER` set, bearer tokens must be JWTs signed with a key from the issuer's JWKS (asymmetric algorithms only — `none` and HMAC are refused), with a matching `iss` and `aud`, and an `exp` that has not passed (30 s clock leeway). JWTs are only accepted in the `Authorization` header, never in `?token=`.
- **Origin checking.** Browser requests from an origin outside `AH_MCP_ALLOWED_ORIGINS` are rejected, which is what stops a malicious web page reaching a loopback server via DNS rebinding. Non-browser clients send no `Origin` and are unaffected.
- **The login proxy is scoped to a one-time secret.** `ah_login` starts a short-lived reverse proxy under a random `/<secret>/` path that only appears in the login URL returned over MCP. Without it the proxy would relay to AH's login host for anyone who could reach the port, and its callback would accept an authorization code from a stranger — AH's flow carries no `state` parameter, so that would let someone bind *their* account to your server. Opening that URL sets an HttpOnly cookie holding the secret, which the login page's root-relative assets (`/login/_next/...`) need to get through; requests without it get a 404, and the callback only answers under the secret path.
- **Tokens on disk.** Written atomically, file mode `0600`, directory `0700`.
- **Prefer the header over `?token=`.** Query strings land in reverse-proxy access logs, browser history and `Referer` headers. Use `Authorization: Bearer <token>` where your client supports it.
- **Put TLS in front for remote deployments.** The server speaks plain HTTP; terminate TLS in nginx/Caddy.

## Token storage

Tokens are stored in the XDG-compliant config directory:

| Platform | Default path |
|---|---|
| Linux | `~/.config/ah-mcp/tokens.json` |
| macOS | `~/Library/Application Support/ah-mcp/tokens.json` |
| Windows | `%AppData%\ah-mcp\tokens.json` |

The directory is created with mode `0700` and the file with mode `0600` (owner read/write only). Tokens are refreshed automatically before every API call — you only need to run `ah_login` once.

Override with `AH_TOKENS_PATH` if needed.

## First login

Just call the **`ah_login`** tool from your AI assistant.

**Local mode** (default — browser opens and the call blocks until login completes):
```
User: log in to ah
Assistant: calls ah_login  ← browser opens automatically
→ (you complete login in browser)
→ "Login successful! Connected as Jan Jansen."  ← single call, no confirmation needed
```

**Remote mode** (`--remote` flag or `AH_REMOTE=true` — URL returned for manual opening):
```
User: log in to ah
Assistant: calls ah_login
→ "Please open this URL in your browser to log in to Albert Heijn:
   https://ah-mcp.example.com/login?..."
→ (complete login in browser)
→ (call ah_login again)
→ "Login successful! Connected as Jan Jansen."
```

No configuration needed for local use.

## Client setup guides

### Claude.ai (web) / Claude Desktop — Streamable HTTP (recommended for remote)

Streamable HTTP uses regular HTTP requests instead of a persistent SSE connection — more stable through proxies and cloud infrastructure.

1. Start the server with `--remote` on your remote machine:
   ```bash
   ./ah-mcp --transport streamable-http --remote
   ```
2. Open Claude → Settings → Connections → Add custom MCP server.
3. Paste the URL: `https://your-server/mcp?token=your-secret-token`

### OAuth with Authentik

Instead of pasting a static token into the URL, the server can act as an OAuth resource server ([MCP authorization spec 2025-06-18](https://modelcontextprotocol.io/specification/2025-06-18/basic/authorization)), with [Authentik](https://goauthentik.io) as the authorization server. Clients that speak MCP OAuth (Claude.ai, Claude Desktop) then sign you in through Authentik.

1. In Authentik, create an **OAuth2/OpenID Provider** and an **Application** for it. Add the client's redirect URI (for Claude: `https://claude.ai/api/mcp/auth_callback`). Note the client ID and the issuer URL shown on the provider page.
2. Start the server:
   ```bash
   AH_MCP_BASE_URL=https://your-server \
   AH_MCP_OAUTH_ISSUER=https://authentik.example.com/application/o/ah-mcp/ \
   AH_MCP_OAUTH_AUDIENCE=<client id> \
   AH_MCP_TOKEN=$(openssl rand -hex 32) \
   ./ah-mcp --transport streamable-http --remote
   ```
   `AH_MCP_TOKEN` is optional here. Set it if you also want a static token, e.g. for Claude Code on the LAN.
3. Add `https://your-server/mcp` as a connector, without `?token=`. If the client asks for an OAuth client ID, use the one from step 1.

What the server does:

- It serves `/.well-known/oauth-protected-resource` (RFC 9728), naming the issuer as the authorization server. This endpoint needs no credentials.
- A request without a valid token gets `401` with `WWW-Authenticate: Bearer resource_metadata="<AH_MCP_BASE_URL>/.well-known/oauth-protected-resource"`, which is how the client discovers where to sign in.
- It checks JWT signatures against the JWKS from Authentik's OpenID discovery document. Keys are cached for an hour, and an unknown `kid` triggers a refetch (at most once a minute), so key rotation needs no restart.

`AH_MCP_BASE_URL` must be the public URL: it is the `resource` in the metadata and the URL in the `401` challenge.

### Claude.ai (web) — SSE (legacy)

1. Start the server on your remote machine with a token set:
   ```bash
   AH_MCP_TOKEN=$(openssl rand -hex 32) ./ah-mcp --transport sse --remote
   ```
   Without `--remote` the server binds loopback only; without `AH_MCP_TOKEN` a non-loopback bind is refused.
2. Open Claude.ai → Settings → Connections → Add MCP server.
3. Paste the SSE URL: `https://your-server/sse?token=your-secret-token`

### Claude Desktop — SSE or stdio

**SSE** (remote server — run with `--remote` and `AH_MCP_TOKEN` on the server side):

```json
{
  "mcpServers": {
    "ah": {
      "url": "https://your-server/sse",
      "headers": { "Authorization": "Bearer your-secret-token" }
    }
  }
}
```

If your client cannot send headers, fall back to `https://your-server/sse?token=your-secret-token` — but note the token will appear in proxy logs.

**stdio** (local binary):

```json
{
  "mcpServers": {
    "ah": {
      "command": "/path/to/ah-mcp",
      "args": ["--transport", "stdio"]
    }
  }
}
```

### Windsurf / Cursor — stdio

Add to your MCP config (usually `~/.codeium/windsurf/mcp_config.json` or `~/.cursor/mcp.json`):

```json
{
  "mcpServers": {
    "ah": {
      "command": "/path/to/ah-mcp",
      "args": ["--transport", "stdio"]
    }
  }
}
```

## Remote server deployment

### systemd setup

1. Create a dedicated user:
   ```bash
   sudo useradd -r -m -d /home/ah-mcp -s /sbin/nologin ah-mcp
   ```
2. Copy the binary:
   ```bash
   sudo cp ah-mcp /usr/local/bin/ah-mcp
   sudo chmod 755 /usr/local/bin/ah-mcp
   ```
3. Create `/home/ah-mcp/.env`:
   ```env
   AH_CALLBACK_HOST=https://ah-mcp.example.com
   AH_MCP_BASE_URL=https://ah-mcp.example.com
   AH_MCP_PORT=3000
   AH_REMOTE=true
   AH_MCP_TOKEN=your-secret-token-here
   ```
4. Install and start the service:
   ```bash
   sudo cp ah-mcp.service /etc/systemd/system/
   sudo systemctl daemon-reload
   sudo systemctl enable --now ah-mcp
   ```
5. Put a reverse proxy (nginx, Caddy) in front to handle TLS.

Tokens are stored automatically at `/home/ah-mcp/.config/ah-mcp/tokens.json` — no `AH_TOKENS_PATH` override needed.

## Available tools

### Authentication

| Tool | Description |
|---|---|
| `ah_login` | Log in via browser OAuth. First call returns a URL; call again after completing login. |
| `ah_logout` | Delete stored tokens to log out or switch accounts. |
| `ah_get_server_info` | Server and appie-go versions, auth state, cache size. |

### Products

| Tool | Description |
|---|---|
| `ah_search_products` | Search products by keyword (Dutch terms preferred). |
| `ah_search_products_bulk` | Search several keywords in one call (max 10). Saves tool-call quota. |
| `ah_get_products_bulk` | Details for several product IDs in one call (max 20). |
| `ah_search_products_filtered` | Search with optional `bonus=true` filter for on-sale items only. |
| `ah_get_product` | Full detail for one product by ID. Add `include_nutritional_info=true` for calories, fat, protein, etc. |
| `ah_get_bonus_offers` | All current bonus/promotional offers. Optional keyword filter. |
| `ah_get_bonus_group_products` | All products in a specific bonus deal group (e.g. "2+1 gratis"). Use `segment_id` from `ah_get_bonus_offers`. |
| `ah_get_last_chance_items` | Vandaag-af / clearance items from a specific store. |
| `ah_search_stores` | Find AH stores near a postal code (or your registered address). |

### Shopping cart (online order)

| Tool | Description |
|---|---|
| `ah_get_cart` | View current online cart: items, quantities, total price. |
| `ah_get_cart_summary` | Cart totals only: item count, price, discount, delivery cost. |
| `ah_update_cart_item` | Set quantity for a product in the cart (0 removes it). |
| `ah_remove_from_cart` | Remove a single product from the cart. |
| `ah_clear_cart` | Remove all items from the cart. Requires `confirm=yes`. |

### Order history & editing

| Tool | Description |
|---|---|
| `ah_get_order_history` | Upcoming delivery orders with status and modifiable flag. |
| `ah_get_past_orders` | Past/delivered orders. |
| `ah_get_order_details` | Full item list for a specific past or upcoming order. |
| `ah_get_frequent_items` | Products you order most often, ranked by frequency. Analyses the 25 most recent orders by default (`max_orders`, up to 100). |
| `ah_reopen_order` | Unlock a submitted order for editing (before closing time). ⚠️ unconfirmed |
| `ah_update_order_items` | Add/change/remove items in a reopened order. ⚠️ unconfirmed |
| `ah_revert_order` | Resubmit a reopened order. **Always call this after `ah_reopen_order`.** ⚠️ unconfirmed |

### Shopping list

| Tool | Description |
|---|---|
| `ah_get_shopping_list` | View your shopping list with item names and IDs. |
| `ah_add_to_shopping_list` | Add products by ID and quantity. |
| `ah_add_free_text_to_shopping_list` | Add a free-text reminder (no product ID needed). |
| `ah_remove_from_shopping_list` | Remove items by product ID or free-text name. |
| `ah_check_shopping_list_item` | Tick or untick a **favourite-list** item. The main Boodschappenlijst returns `listItemId=0` for every item, so its items cannot be ticked here — do that in the AH app. |
| `ah_clear_shopping_list` | Remove all items from the list. Requires `confirm=yes`. |
| `ah_shopping_list_to_order` | Move all unchecked product items from your list to the cart. |
| `ah_get_favorite_lists` | List all named favourite lists with IDs. |
| `ah_add_to_favorite_list` | Add products to a named favourite list. |
| `ah_remove_from_favorite_list` | Remove products from a named favourite list. |

### Receipts

| Tool | Description |
|---|---|
| `ah_get_receipts` | List recent in-store receipts (kassabonnen) with dates and totals. |
| `ah_get_receipt_details` | Full item breakdown, discounts, and payment method for one receipt. |

### Member

| Tool | Description |
|---|---|
| `ah_get_member_profile` | Your name, email, and bonus card number. |

## Troubleshooting

**Port conflict on 9876 or 3000**
Change via `AH_CALLBACK_PORT` or `AH_MCP_PORT` in your `.env`.

**Login timeout after 5 minutes**
The OAuth flow timed out. Call `ah_login` again and complete the browser flow faster.

**Token issues / expired session**
Call `ah_logout` then `ah_login`. Or delete `tokens.json` manually:
- Linux: `rm ~/.config/ah-mcp/tokens.json`
- macOS: `rm ~/Library/Application\ Support/ah-mcp/tokens.json`
- Windows: `del %AppData%\ah-mcp\tokens.json`

**Last-chance items require a store**
`ah_get_last_chance_items` needs a store ID or postal code — bargain items are store-specific. Provide `store_id` or `postal_code` as a parameter.

**"refusing to listen ... without AH_MCP_TOKEN or AH_MCP_OAUTH_ISSUER"**
You asked for a network bind (`--remote`, `AH_REMOTE=true` or `AH_MCP_BIND`) without auth. Set `AH_MCP_TOKEN` and/or configure OAuth, or drop those to bind loopback only.

**"rejected bearer token" in the log**
The reason follows. `invalid audience` means `AH_MCP_OAUTH_AUDIENCE` does not match the token's `aud` (for Authentik, the client ID). `reports issuer ... but AH_MCP_OAUTH_ISSUER is ...` means the configured issuer differs from Authentik's, which is usually a missing trailing slash.

**403 "Forbidden origin"**
A browser called the server from an origin that is not allowed. Add it to `AH_MCP_ALLOWED_ORIGINS` (comma separated), or set that to `*` to disable the check.

**Client cannot reach the server on another machine**
By default the server only listens on `127.0.0.1`. Start it with `--remote` (plus `AH_MCP_TOKEN`) to listen on all interfaces.

**"Not logged in" error**
Run `ah_login` first. In local mode the browser opens automatically; in remote mode (`--remote` / `AH_REMOTE=true`) open the URL the assistant returns.

## Acknowledgements

**ah-mcp** is built on top of [**appie-go**](https://github.com/gwillem/appie-go) — a Go client library for the Albert Heijn mobile API by [@gwillem](https://github.com/gwillem). It provides the authenticated HTTP client, all API call implementations (product search, bonus offers, orders, shopping lists, member profile, bargain items), and the OAuth token format. This project depends on it as a library. Note that `go.mod` currently pins a fork (`github.com/celerex/appie-go`) via a `replace` directive to pick up Belgium support; `go.sum` pins the exact revision.

The OAuth reverse-proxy login flow in `auth.go` is inspired by the approach in appie-go's `login.go`, adapted to return a URL string rather than open a browser — making it safe for server-side MCP use.
