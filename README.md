# PTV Commute

Personal Melbourne train timetable tool covering two commute routes:

- **Sandown Park ↔ Town Hall** — direct Cranbourne/Pakenham line
- **Mentone ↔ Town Hall** — Frankston line to Caulfield, transfer to Cranbourne/Pakenham (≥5 min connection)

## Architecture

```
CloudFront → S3 (SPA)
                │
                ▼
API Gateway → Lambda (Go, ARM64)
                │
                ├── PTV API v3 (timetable, realtime)
                └── DynamoDB (historical actuals)
                        ▲
              EventBridge (every 3 min, collector)
```

- **Lambda** handles both API Gateway (HTTP) and EventBridge (scheduler) events via a single `universalHandler`
- **Collector** runs 24/7, storing every train that has a live PTV estimate; refreshes arrival estimates for in-transit runs every 3 min
- **SPA** detects GPS location, picks nearest station (Town Hall / Mentone / Sandown Park), auto-refreshes every 60s

## Prerequisites

- Go ≥ 1.24
- Terraform ≥ 1.6
- AWS credentials with permissions for Lambda, API Gateway, DynamoDB, S3, CloudFront, IAM, EventBridge, CloudWatch
- PTV API key — request at [ptv.vic.gov.au/footer/data-and-reporting/datasets/ptv-timetable-api](https://www.ptv.vic.gov.au/footer/data-and-reporting/datasets/ptv-timetable-api/)

## Deploy

```bash
cd terraform
cp terraform.tfvars.example terraform.tfvars   # fill in PTV credentials
terraform init
terraform apply
```

Terraform cross-compiles the Lambda, zips it, uploads to AWS, and deploys the SPA to S3/CloudFront. Outputs:

```
api_url = "https://<id>.execute-api.ap-southeast-2.amazonaws.com/"
spa_url = "https://<id>.cloudfront.net"
```

## API Endpoints

All endpoints are on the API Gateway base URL.

### `GET /timetable`

Plan a journey. Three modes:

| Mode | Parameters |
|------|-----------|
| Outbound arrive-by | `destination=(mentone\|sandown_park)` + `arrive_by=HH:MM` |
| Outbound depart-at | `destination=(mentone\|sandown_park)` + `depart_at=HH:MM` |
| Inbound depart-at  | `from=(mentone\|sandown_park)` + `depart_at=HH:MM` |

Optional: `date=YYYY-MM-DD` (default today), `realtime=true` (use live estimates).

### `GET /travel-times`

Departure scatter data for the morning (07:00–09:30) and evening (16:00–18:00) commute windows.

`route=(sandown_park_inbound|sandown_park_outbound|mentone_inbound|mentone_outbound)`  
Optional: `date=YYYY-MM-DD`

### `GET /history`

Historical actuals collected by the EventBridge collector.

`route=<route>` + optional `date=YYYY-MM-DD`

Returns `{ route, date, trains: [{ run_ref, depart_scheduled, depart_actual, arrive_scheduled, arrive_actual, travel_min_scheduled, travel_min_actual, delay_min }] }`

## Local CLI

```bash
cd lambda
go run . -dest mentone -arrive 17:30 [-date YYYY-MM-DD] [-realtime]
go run . -dest sandown_park -arrive 08:45
go run . -from mentone -depart 08:00
```

## SPA

Open `spa_url` in Safari on iOS or any browser. Add to Home Screen for PWA install. The app:

- Detects nearest station via GPS (haversine distance)
- Shows next train with urgency colour (>6 min green, 3–6 orange, <3 red)
- **Live tab**: current timetable with auto-refresh
- **History tab**: delay chart + table for any route/date from DynamoDB

## DynamoDB Schema

Table: `ptv-timetable-departures`  
PK: `YYYY-MM-DD#<route>` / SK: `<run_ref>` / TTL: 30 days

Key attributes: `depart_scheduled`, `depart_actual`, `arrive_scheduled`, `arrive_actual`, `travel_min_scheduled`, `travel_min_actual`, `delay_min`, `connect_run_ref` (transfer routes only).
