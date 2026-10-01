# ipRoyal

Go HTTP server built with [Gin](https://github.com/gin-gonic/gin) exposing a customer-scoped scrape API with prepaid request balances, a per-customer concurrency cap, and bounded outbound fetches.

## Endpoints

- `GET /v1/scrape?url=...&format=html|json&token=...` - Scrape a URL as a customer.
- `POST /v1/users` - Create a customer and receive its token. Requires the admin token.
- `POST /v1/users/:id/refill` - Top a customer's balance back up. Requires the admin token. **Not implemented, returns 501.**

## Quick start

The service seeds a demo customer on startup when `DEMO_TOKEN` is set, so it is usable without a create-user round trip:

```bash
export ADMIN_TOKEN=your_admin_token
export DEMO_TOKEN=demo_abc123      # any value you choose; this is the demo customer's token
export DEMO_BALANCE=100            # optional, defaults to MaxRequests (100)
go run .
```

```bash
curl "http://localhost:8080/v1/scrape?url=https://example.com&format=html&token=demo_abc123"
```

The seed token is read from the environment rather than defaulted, so no working credential is baked into the binary. If `DEMO_TOKEN` is unset the seed is skipped and `POST /v1/users` is the only way to mint credentials.

## Scraping

The customer token travels as the `token` query parameter.

| Parameter | Required | Notes |
|-----------|----------|-------|
| `url`     | yes | Only `http` and `https` schemes. Private, loopback, and link-local targets are refused. |
| `format`  | no  | `html` or `json`. Defaults to `html`. |
| `token`   | yes | The customer id. |

`format=html` returns the raw upstream body with the upstream status code and `Content-Type`. `format=json` returns the content plus metadata:

```json
{
  "customer": "demo",
  "requests_left": 99,
  "url": "https://example.com",
  "final_url": "https://example.com/",
  "status_code": 200,
  "content_type": "text/html; charset=utf-8",
  "size_bytes": 1256,
  "truncated": false,
  "duration_ms": 143,
  "fetched_at": "2026-10-01T14:31:03Z",
  "content": "<!doctype html>..."
}
```

Every authorized response carries `X-Requests-Remaining`.

Note that a token in the query string lands in access logs, browser history, and any intermediary logs. Use TLS in production, and be aware gin's logger records the request URL verbatim.

## Prepaid balance

Each customer starts with `MaxRequests` (100) requests, which is also the hard ceiling. `noRequests` drops by one for every scrape that **succeeds**.

`CustomerManager.Acquire` takes one unit and one in-flight slot atomically before the fetch; `CustomerManager.Release` frees the slot and refunds the unit when the scrape failed. The net effect is charge-per-success, while both limits stay hard under concurrency.

- Failed and timed-out fetches are refunded, so they cost nothing.
- Requests rejected during validation (missing `url`, bad `format`, private or non-resolving target) are refused *before* anything is reserved, so malformed calls never consume balance.
- A release without a matching acquire is a no-op, so a stray or duplicated call cannot manufacture credit.
- An upstream 404 or 500 counts as success, since content was retrieved and returned. Only transport failures, timeouts, and validation rejections are refunded.

## Status codes

| Situation | Status | Body |
|-----------|--------|------|
| Scrape delivered | upstream status, e.g. `200` | raw body or metadata envelope |
| Malformed request (bad `url`, `format`, private target) | `400` | `{"error": "..."}` |
| Missing or unknown token | `401` | `{"error": "invalid token"}` |
| Balance exhausted | `402` | `{"error": "no requests left on this account", "requests_left": 0}` |
| Over the in-flight cap | `429` + `Retry-After` | `{"error": "too many concurrent scrapes for this customer", "max_in_flight": 4, "requests_left": 95}` |
| Upstream timed out | `504` | `{"error": "upstream request timed out"}` |
| Upstream unreachable | `502` | `{"error": "failed to fetch url: ..."}` |

`402` and `429` are deliberately distinct: one is a credit problem needing a top-up, the other is a transient concurrency signal meaning back off and retry. `429` also carries `Retry-After`.

## Concurrency cap

`maxInFlight` (4) limits how many scrapes one customer may have outstanding at once. Without it a single customer with a large balance could occupy every connection in the fetch transport and starve everyone else.

The surplus is rejected immediately rather than queued, so a client learns right away instead of waiting behind a hung target. The cap is per customer, and `429` responses are not charged.

## Slow-target protection

- `fetchTimeout` (15s) is the firm ceiling per scrape, applied as both a request context deadline and `http.Client.Timeout`. The context covers DNS resolution, which `Client.Timeout` alone does not always reach; `Client.Timeout` covers the body read, which a context deadline alone would not bound once headers have been received. Whichever fires first aborts the request.
- `ResponseHeaderTimeout` (10s) cuts off a target that connects but never responds.
- `TLSHandshakeTimeout` (10s) covers a stalled handshake.
- The dialer timeout fails fast on an unreachable host.
- A timeout releases the customer's in-flight slot, so a hung site cannot hold it open. Verified by `TestStalledScrapeFreesInFlightSlot`, which confirms slots drain to zero and the balance is untouched.
- Idle connections are bounded and reaped (`MaxIdleConns`, `MaxIdleConnsPerHost`, `IdleConnTimeout`) so one-off scrapes cannot accumulate sockets.
- Response bodies are capped at 5MB. Truncated HTML responses return `text/plain` with `X-Scrape-Truncated: true`.

## Security behavior

- Blocks non-public addresses (loopback, private ranges, link-local, unspecified, multicast) at DNS resolution *and* at connect time, which also covers DNS rebinding between validation and the connection.
- Rejects URLs with embedded credentials; follows at most 5 redirects, re-validating each hop.
- Customer tokens are 256 bits of entropy, generated server-side, returned once, and stored only as SHA-256 hashes, so the in-memory table cannot be replayed against the API.
- `POST /v1/users` and the refill route require the admin token via `Authorization: Bearer`.

## Creating a customer

```bash
curl -X POST http://localhost:8080/v1/users \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"name": "acme"}'
```

```json
{
  "id": "9f2c1e...",
  "name": "acme",
  "created_at": "2026-10-01T14:31:02Z",
  "requests": 100,
  "message": "the id is the token, use it as ?token=<id>. it is not retrievable later"
}
```

The `id` **is** the customer's token, so there is nothing else to store.

## Refilling (not implemented)

`POST /v1/users/:id/refill` is stubbed and returns `501 Not Implemented`. It never touches the balance, so it is safe to leave in place while the design is settled. Open questions recorded in `handlers/handler.go`:

- body shape: absolute (`set to N`) versus additive (`add N`), and whether an empty body means `reset to the full allowance`;
- whether an exhausted customer may be topped back up at all, given that `MaxRequests` is both the starting balance and the ceiling, so any top-up is capped at exactly what a fresh customer holds;
- whether refills are audited, and which caller identity is recorded.

## Configuration

| Environment Variable | Description | Default |
|---------------------|-------------|---------|
| `ADMIN_TOKEN`       | Bearer token for `POST /v1/users` and the refill route | (required) |
| `DEMO_TOKEN`        | Token for the seeded demo customer. Unset skips the seed. | unset |
| `DEMO_BALANCE`      | Starting balance for the demo customer | `100` |
| `ADDR`              | Listen address | `:8080` |

Compile-time constants: `MaxRequests` (100, both the starting balance and the hard ceiling) and `MaxInFlight` (4) in `cmManager/customerManager.go`, `FetchTimeout` (15s) and `MaxBodyBytes` (5MB) in `scraper/scrape.go`.

## Package layout

| Package | Responsibility |
|---------|----------------|
| `main` | Wiring only: config, demo seed, route registration, listener. No business logic. |
| `auth` | Admin bearer-token middleware and required-env lookup. |
| `cmManager` | Customer store, token generation and lookup by hash, balances, in-flight slots, `Acquire`/`Release`. No HTTP knowledge. |
| `scraper` | Outbound fetch: target validation, SSRF guard, bounded client, body cap, timeout. No customer knowledge. |
| `handlers` | HTTP surface. Glues the three above and owns request/response shapes. |

The dependency direction is one way: `main` → `handlers` → {`auth`, `cmManager`, `scraper`}. `cmManager` and `scraper` know nothing about HTTP or about each other, which is what keeps their tests runnable without a server.

`handlers.validateTarget` and `handlers.newFetchClient` are indirections over the `scraper` package. The serving path always uses the real ones; the tests swap them out to reach a loopback `httptest` server that the SSRF guard would otherwise reject.

## Limitations

Customers live in memory only, so tokens and balances are lost on restart, including the demo customer. Wire `CustomerManager` to a database when you need persistence. There is also no global concurrency ceiling, only the per-customer one, so many customers together can still open more connections than the transport expects.

## Development

```bash
go build ./...
go vet ./...
gofmt -w .
go test ./...
go test -short ./...   # skips the timing-sensitive timeout tests
```

The race detector needs cgo, so `go test -race` requires a C toolchain.
