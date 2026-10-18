# OrcaBox managed gateway

This fork extends New API v1.0.0-rc.44 (`633b4c5b190dc1af99a2814391354d86948e7943`) for a website-managed prepaid API service. It is under the repository's existing AGPL-3.0 license; upstream copyright and notices remain in place.

Corresponding source: https://github.com/fanfanliu964-web/OrcaBox-NewAPI/tree/codex/orcabox-managed-gateway

Users interacting with the deployed gateway can obtain this source without logging in or paying. The OrcaBox website footer and API `/source` link identify this offer. The release record must identify the exact commit and image digest used for deployment.

## Managed behavior

- The website grants one 100-credit, 30-day entitlement per unique paid order. PostgreSQL transactions protect grants, expiry, refunds, rotation generations, reservations and settlement. Native wallet quota is not the customer's managed balance.
- Authenticated `/v1/models`, `/v1/responses`, `/v1/chat/completions` and `/v1/usage` share account-level model, balance, RPM, TPM and concurrency constraints. Internal operations require a separate service credential and must not be publicly proxied. New API management routes remain private.
- Credits are integer millionths. Published, immutable price snapshots settle original supplier usage without double-charging cache or reasoning tokens. Supplier context overhead and bounded local image decoding contribute to the admission reserve.
- Missing usage remains frozen for up to 24 hours. Unknown costs then belong to the platform. Numeric settlement spooling survives database outages. Generations are not automatically retried after forwarding.
- Supplier overruns lock the model and retain platform loss in the audit and test budget. An operator can reopen only after publishing a newer price version, checking current supplier prices and recording a review reference.
- Supplier error bodies and SSE error fields are filtered before reaching customers; original numeric usage is observed before filtering. Full credentials, user messages and attachment content must not appear in logs.

## Build and verification

`Dockerfile.orcabox` pins the official base digest and replaces its executable with a statically linked Go build. Build the executable with Go 1.26.5, `CGO_ENABLED=0`, `go build -trimpath -o .artifacts/new-api .`, then build the Dockerfile. Do not commit `.artifacts`, runtime state or credentials.

Run `go test ./orcabox ./relay ./relay/channel` with PostgreSQL test configuration for concurrency coverage. Source tests also cover key-rotation races, historical grant metadata migration, exact billing, refunds, encryption and supplier-error filtering. Actual HTTP, fault recovery, capacity and provider acceptance scripts live in the separate OrcaBox website repository.

The synthetic and real-provider test environments are separate. Do not expose either as production. Actual launch additionally requires fresh runtime credentials, HTTPS, a production database and restore drill, a verified live payment and Windows/macOS client acceptance. Installation and a successful test chat do not establish those conditions.
