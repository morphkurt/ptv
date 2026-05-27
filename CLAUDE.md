# Claude context — PTV Commute

## Project layout

```
lambda/          Go source (single binary, handles both Lambda and local CLI)
  main.go        universalHandler, apiHandler, CLI flag parsing
  journey.go     stop/route/direction constants, PlanJourney / PlanReturnJourney
  travel_times.go GetTravelTimes, fetchDepsInWindow, parallelStopTimes, findBothStopTimesInPattern
  collector.go   EventBridge collector — stores/updates DynamoDB actuals
  history.go     handleHistory, QueryHistory, strVal/intVal helpers
  ptv_client.go  HMAC-SHA1 signing, GetDepartures, GetPattern, GetDisruptions

terraform/
  main.tf        build trigger (null_resource), IAM role, Lambda, API Gateway routes, CloudWatch
  dynamodb.tf    DynamoDB table, IAM policy, EventBridge rule/target/permission, /history route
  spa.tf         S3 bucket, CloudFront OAC distribution, S3 object uploads (index.html, manifest, sw.js)
  variables.tf   aws_region, function_name, ptv_dev_id, ptv_api_key

dashboard/app/
  index.html     Single-file SPA — all CSS, JS, HTML inline; no build step
```

## Deploy

```bash
cd terraform && terraform apply
```

Terraform cross-compiles (`GOOS=linux GOARCH=arm64 CGO_ENABLED=0`) via `null_resource` local-exec triggered by `sha1` hash of all `.go` source files. `source_code_hash` on the Lambda resource uses the same hash (not `filebase64sha256`) to avoid a two-step apply.

After changing `index.html`, also invalidate CloudFront:
```bash
CF_ID=$(aws cloudfront list-distributions --query "DistributionList.Items[?Origins.Items[0].DomainName | contains(@,'ptv-timetable-spa')].Id" --output text)
aws cloudfront create-invalidation --distribution-id "$CF_ID" --paths "/index.html"
```

## PTV API quirks

- **HMAC-SHA1 signing**: signature is `UPPERCASE(hex(hmac-sha1(path+devid, apiKey)))`. The uppercase matters — lowercase is rejected.
- **devid must be in the URL path**, not just the signature input. Append `&devid=<id>` before signing, then append `&signature=<sig>` after.
- **All times are UTC strings** (`"2026-05-27T10:15:00Z"`). Rebase to the journey date in Melbourne TZ using `rebaseToDate()` — the API always returns today's schedule regardless of the `date_utc` query parameter.
- **`estimated_departure_utc`** is only present when PTV has a live GPS estimate. Absence means scheduled-only. The collector only stores rows where this field is non-empty on the origin leg (`dep.hasEst == true`).
- **Frankston line does not stop at Town Hall** — it terminates at Flinders Street. Mentone ↔ Town Hall requires a transfer at Caulfield onto the Cranbourne/Pakenham line (≥5 min connection time enforced).
- **Replacement buses** (e.g. evening engineering works) appear under the same route IDs in the departures endpoint but typically have no `estimated_departure_utc`, so the collector skips them.

## Lambda handler pattern

`universalHandler(ctx, json.RawMessage)` dispatches on the presence of `rawPath`:
- **API Gateway v2**: `rawPath` is present → unmarshal to `APIGatewayV2HTTPRequest`, call `apiHandler`
- **EventBridge**: no `rawPath` → call `Collect()`

`apiHandler` always sets `Access-Control-Allow-Origin: *` on the response. Do not add a `cors_configuration` block to the API Gateway resource — it overrides Lambda headers and breaks `null` origins (file:// local testing).

## Collector design

- Runs every 3 minutes via EventBridge `rate(3 minutes)` — no time-of-day restriction.
- At each tick: first `updateInFlightArrivals`, then store new upcoming departures.
- **Direct routes** (Sandown Park ↔ Town Hall): `storeDirect` — single run_ref covers origin→dest.
- **Transfer routes** (Mentone ↔ Town Hall): `storeTransfer` — two legs, two run_refs. The origin run_ref is the DynamoDB SK. `connect_run_ref` stores the leg-2 run_ref so `refreshInFlightRoute` can look up the destination stop in the connecting train's pattern, not the origin train's (which doesn't serve Town Hall / Mentone).
- In-flight window: `depart_scheduled <= now <= arrive_scheduled + 30 min`. After that, the row is finalised.
- `UpdateItem` only touches `arrive_actual` and `travel_min_actual` — other fields (delay_min, collected_at, etc.) are set once at initial `PutItem`.

## DynamoDB

- PK: `YYYY-MM-DD#<route>` / SK: `<run_ref>` — one item per run per day per route.
- `PutItem` overwrites on repeat collections before departure (keeps latest estimate).
- `UpdateItem` used for in-flight arrival refreshes.
- IAM: `PutItem`, `UpdateItem`, `Query`, `GetItem`.
- TTL: 30 days (`ttl` attribute, epoch seconds).

## SPA notes

- **No build step** — `dashboard/app/index.html` is self-contained (inline CSS + JS).
- API URL is hardcoded as `HARDCODED_API` constant; `localStorage.ptv_api_url` overrides it via Settings.
- Station coordinates: Town Hall (-37.8143, 144.9667), Mentone (-37.9831, 145.0636), Sandown Park (-37.9443, 145.1013).
- Chart.js loaded from CDN (`chart.js@4.4.0`) for the History tab delay bar chart.
- History tab switches view via `body.view-history` CSS class — avoids fighting inline `display:none` on station bar and time controls.

## Stop / Route / Direction constants (journey.go)

```go
stopTownHall    = 1235
stopCaulfield   = 1036
stopMentone     = 1122
stopSandownPark = 1172

routeCranbourne = 4
routePakenham   = 11
routeFrankston  = 6

dirCity       = 1
dirCranbourne = 3
dirPakenham   = 10
dirFrankston  = 5
```

## Testing

`lambda/journey_test.go` covers the journey planning logic. Run with `go test ./...` from `lambda/`.  
`lambda/test/` contains one-off CLI tools for exploring the PTV API (not part of the Lambda binary).
