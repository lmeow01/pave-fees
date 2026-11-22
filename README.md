# Fees API

Bills that accrue fee line items over a billing period and are totalled when
closed. Go, [Encore](https://encore.dev), [Temporal](https://temporal.io),
Postgres.

The design, the trade-offs and what was left out are in [specs.md](specs.md).

## Prerequisites

| Tool | Used for | Install |
|---|---|---|
| Go 1.26+ | Building | https://go.dev/dl |
| Encore CLI | Running and testing the app | `curl -L https://encore.dev/install.sh \| bash` |
| Docker | Encore runs Postgres in a container | https://docs.docker.com/get-docker |
| Temporal CLI | Local Temporal dev server | https://docs.temporal.io/cli#install |
| Task | Task runner | https://taskfile.dev/installation |

No Encore account is needed; the app is not linked to the Encore platform.

## Run it

In one terminal, start Temporal (UI at http://localhost:8233):

```sh
task temporal
```

In another, start the API (http://localhost:4000, Encore's dashboard at
http://localhost:9400):

```sh
task run
```

Encore creates the database and applies the migration on start.

## Try it

Amounts are strings of minor units: `"1050"` in USD is $10.50.

```sh
# Create a USD bill (period_end is when it closes itself)
BILL=$(curl -s -X POST localhost:4000/v1/bills \
  -H 'Content-Type: application/json' \
  -H "Idempotency-Key: $(uuidgen)" \
  -d '{"customer_id": "customer_42", "currency": "USD", "period_end": "2030-01-01T00:00:00Z"}' \
  | jq -r .id)

# Accrue two fees
curl -s -X POST localhost:4000/v1/bills/$BILL/line-items \
  -H 'Content-Type: application/json' -H "Idempotency-Key: $(uuidgen)" \
  -d '{"description": "SWIFT transfer fee", "amount": "1050", "currency": "USD"}' | jq

curl -s -X POST localhost:4000/v1/bills/$BILL/line-items \
  -H 'Content-Type: application/json' -H "Idempotency-Key: $(uuidgen)" \
  -d '{"description": "Account maintenance", "amount": "295", "currency": "USD"}' | jq

# See what has accrued
curl -s localhost:4000/v1/bills/$BILL | jq

# Page through the line items, one per page; pass next_page_token to continue
curl -s "localhost:4000/v1/bills/$BILL/line-items?page_size=1" | jq

# List the customer's bills
curl -s "localhost:4000/v1/bills?customer_id=customer_42" | jq

# Close: total_amount is "1345", with both line items. A bill with more than
# 100 line items returns the first 100 and a next_page_token for the rest.
curl -s -X POST localhost:4000/v1/bills/$BILL/close | jq

# Rejected: the bill is closed
curl -s -X POST localhost:4000/v1/bills/$BILL/line-items \
  -H 'Content-Type: application/json' -H "Idempotency-Key: $(uuidgen)" \
  -d '{"description": "Late fee", "amount": "100", "currency": "USD"}' | jq
```

Other things worth trying: a line item in `GEL` on the USD bill (rejected),
repeating a request with the same `Idempotency-Key` (same response, nothing
added), and a bill with a `period_end` a minute away (it closes itself). The
bill's workflow, with every accepted line item and the close, is visible in
the Temporal UI under the bill's ID.

## API reference

The endpoints, with examples and the error table, are in section 4 of
[specs.md](specs.md). While `task run` is up, Encore's dashboard at
http://localhost:9400 has an API explorer for calling them. For Swagger UI or
Postman, Encore generates an OpenAPI 3 document from the code:

```sh
encore gen client --lang=openapi --output=openapi.json
```

## Verify

```sh
task            # build, lint and test: what CI runs
task build      # compile the Encore app and check that it boots
task lint       # golangci-lint and Temporal's workflow determinism check
task test       # all tests; needs Docker
task test-unit  # money and workflow tests with the race detector; needs nothing but Go
task record-histories -- v2  # record workflow histories for the replay test
```

`task record-histories` needs the Temporal server that `with-temporal.sh`
starts, so stop your own `task temporal` server before running it.

`task build` and `task test` start an in-memory Temporal dev server for the
run if none is listening on `localhost:7233`, and stop it afterwards. That
server is told to suggest continue-as-new after 40 history events, so the
tests cover bills whose workflow rolls over to a new run. Tests run against
your own `task temporal` server pass too, but do not exercise the rollover.

The end-to-end tests run without `-race`: the race detector crashes at startup
under the Go toolchain bundled with the Encore CLI v1.58.9. The workflow and money
packages do not depend on Encore, so `task test-unit` runs them with `-race`
on the standard toolchain.

CI (`.github/workflows/ci.yml`) runs `task build`, `task lint` and `task test`
on pushes to `main` and on pull requests.

## Layout

```
fees/
  money/        Currency and Amount (integer minor units), strict parsing
  billflow/     Temporal workflow for one bill, its updates and activities;
                testdata/ holds recorded histories the workflow must replay
  migrations/   Postgres schema
  service.go    Encore service: Temporal client and worker
  overdue.go    Cron sweep that closes bills whose workflow never started
  bills.go      Create, list, get and close endpoints
  line_items.go Add and list line item endpoints
  pagination.go Page size and page token rules
  middleware.go Keeps server-side error details out of responses
  store.go      Postgres queries
  *_test.go     End-to-end tests through the endpoints and the store
  encore.gen.*  Generated by Encore; committed so linters and editors can type-check
specs.md        Requirements, API, design and deep dives
```
