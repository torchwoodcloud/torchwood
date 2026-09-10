# Spec: Dead-Letter Replay Tool (`torchwood admin outbox replay`)

## Problem Statement

`document_events_outbox_dead` accumulates events that failed to be enqueued to the realtime transport after `maxOutboxAttempts` (10) retries. Currently the only observability is the `torchwood_outbox_dead` gauge (non-zero means manual intervention needed), but there is no safe, audited way to re-drive a dead-lettered event back into the delivery pipeline. Operators must manually inspect the dead table and re-insert rows, risking duplicate `event_id`, missing audit trails, and bypassing project/permission checks. A `fail-closed` replay path that reuses the existing outbox→transport→published_at pipeline is needed.

## Solution

Provide an admin-only replay path that atomically moves a dead-letter row back into `document_events_outbox` (resetting `attempts/available_at/dispatched_at`) and reuses the normal `OutboxWorker` claim/XADD flow, with full audit and scope coverage. The CLI `torchwood admin outbox replay --event-id <id>` (and `--project-id` filter) drives a new gRPC method `OutboxService/ReplayDeadLetter`.

## User Stories

1. As a platform operator, I want to list dead-lettered events (`admin outbox list-dead --project-id <pid> --limit 20`), so that I can see which events are stuck and why (`last_error`, `attempts`, `created_at`).
2. As a platform operator, I want to replay a single dead-lettered event by `event_id`, so that a transient transport failure does not permanently lose a document change.
3. As a platform operator, I want to replay all dead-lettered events for a project, so that a region-wide outage can be recovered without per-event manual work.
4. As a platform operator, I want replay to be idempotent (replaying an already-replayed `event_id` returns success without duplicate delivery), so that retries are safe.
5. As a platform operator, I want replay to be audited (who, when, which `event_id`, result), so that recovery actions are traceable.
6. As a platform operator, I want replay to require `outbox:write` scope / `owner|admin` console role, so that only authorized admins can re-drive events.
7. As a system, I want replay to reuse the existing `published_at` lifecycle (reset `published_at=NULL` in outbox, let `OutboxWorker` claim/XADD and `Subscriber` mark `published_at`), so that delivery semantics stay `at-least-once` and dedup via `Hub` 5min window still applies.
8. As a system, I want replay to reset `attempts` to 0 and `available_at` to `NOW()`, so that the event is immediately eligible for the next `pollOnce` cycle (without waiting for `redispatch` window).
9. As a system, I want replay to fail closed if the `event_id` does not exist in `dead` or the `project_id` mismatches, so that cross-project replay is prevented.
10. As a developer, I want the replay path to be covered by `buf breaking` and `assertRegisteredMethodsHaveAuthz`, so that the new RPC's authz is not silently bypassed.

## Implementation Decisions

- **Proto**: Add `proto/server/v1/outbox.proto` (or extend `proto/server/v1/databases.proto` if keeping database-adjacent) with `service OutboxService { rpc ListDeadLetters(ListDeadLettersRequest) returns (ListDeadLettersResponse); rpc ReplayDeadLetter(ReplayDeadLetterRequest) returns (ReplayDeadLetterResponse); }`. `ReplayDeadLetterRequest` carries `event_id` (required) and `project_id` (optional, for cross-check). `ReplayDeadLetterResponse` returns the revived `event_id` and `available_at`. `ListDeadLetters` is paginated via `shared.v1.ListRequest` (page_size/page_token, filter/order_by rejected as per W-K). Both methods are `ACCESS_API_KEY` at service level but `method_auth` will be `ACCESS_PERMISSION` with `permissions: ["outbox:write"]` (or `["console"]` if keeping console-only, to be decided with W-K `ACCESS_CONSOLE`).

- **Domain**: `internal/domain/events` adds `OutboxDeadLetter` struct (mirror of `model.DocumentEventsOutboxDead`) and `OutboxRepository` port extension `ListDeadLetters(ctx, projectID string, q ListQuery) / ReplayDeadLetter(ctx, eventID, projectID string) error`. `Replay` is a single transaction: `SELECT * FROM dead WHERE event_id=? FOR UPDATE` → `INSERT INTO outbox (event_id, project_id, topic, channel, payload, attempts=0, available_at=NOW(), dispatched_at=NULL, created_at)` → `DELETE FROM dead WHERE event_id=?` (or use `INSERT ... SELECT` + `DELETE` in one Tx, as `failRow` does the reverse). Use `bun.In` for single row.

- **App**: `internal/app/events` adds `OutboxAdmin` use-case with `ListDeadLetters`/`ReplayDeadLetter` that checks `RequireServerWriteActor` (or stricter `RequirePlatformAdmin` if outbox is platform-level; to be decided – current dead table is per-project, so `owner|admin` of the project should suffice, matching `APIKeysService` `owner|admin` semantics).

- **Infra**: `internal/infra/bun/bunrepo/outbox_repo.go` implements the two methods via `clients.Database` (with per-statement `5s` timeout as per W-H). `internal/infra/events` already owns `OutboxWorker`; no change to worker logic – replayed row is naturally picked up by next `pollOnce`.

- **API**: `internal/api/servergrpc/outbox.go` thin handler (like `projects.go`), `projectID` from `Principal`, `WithAuditResource("outbox/dead/"+eventID)`. Wire via `internal/app/provides.go` (`events.NewOutboxAdmin`) and `internal/infra/server/grpc.go` (`collectMethodsByAccess` includes new file descriptor, `assertRegisteredMethodsHaveAuthz` already covers it).

- **CLI**: `cmd/torchwood/cmd/outbox.go` adds `torchwood admin outbox list-dead` and `replay` commands using `sdk/go/server` `InvokeJSON` (no direct `genproto` import, per `cmd/torchwood` guard). `sdk/go/server` adds `OutboxService` client (like `projects.go`).

- **Audit**: `audit` interceptor already records `Action=FullMethod` and `ResourceID` from `WithAuditResource`; no new audit code needed beyond handler's `WithAuditResource`.

- **Metrics**: On successful replay, `outboxDead` gauge will drop on next `cleanupOnce`/`Count`; no new metric needed (reuse `outboxPublishTotal` for the subsequent XADD).

## Testing Decisions

- **What makes a good test**: Only external behavior – HTTP/gRPC status, DB row movement, audit entry, and metric – not internal channel/Tx details. Use `testutil.SetupTestDB` + `bun` + `miniredis` for transport where needed (as `outbox_worker_test.go` does).

- **Modules to test**:
  - `internal/infra/bun/bunrepo` – `TestOutboxRepo_ReplayDeadLetter` (insert dead row, replay, assert outbox has row with `attempts=0` and dead empty; replay idempotency; project mismatch -> `InvalidArgument`/`NotFound`).
  - `internal/app/events` – `TestOutboxAdmin_Replay` (platform admin vs member, project mismatch, not found).
  - `internal/api/servergrpc` – `TestOutboxService_Replay` (authz: `owner|admin` ok, `member|viewer` denied, API key with `outbox:write` ok).
  - `cmd/torchwood` – `TestBuildOutboxReplayRequest` (like `storage_test.go`).

- **Prior art**: `internal/infra/events/outbox_worker_test.go` (claim/dispatch/failRow), `internal/api/servergrpc/projects_test.go` (List pagination), `internal/infra/auth/validator_test.go` (admin role), `cmd/torchwood/cmd/storage_test.go` (build request).

## Out of Scope

- Automatic retry of dead letters (no cron); operator must explicitly replay.
- Bulk replay with filter `WHERE last_error LIKE ...` (can be added later as `ReplayDeadLettersByProject`).
- Replaying directly to transport (bypassing outbox); always go via outbox to keep `published_at` lifecycle.
- New `ACCESS_CONSOLE` AccessLevel – if decided in W-K, this spec will be updated to use it instead of `ACCESS_PERMISSION`/`outbox:write`.

## Further Notes

- **Seams**: Highest seam is `internal/domain/events.OutboxRepository` (port) + `shared.Queue` (already used by `OutboxWorker`). The gRPC handler is thin; the CLI uses `sdk/go/server` `InvokeJSON` (no `genproto` import, per guard). No new low-level seams needed.

- **Idempotency**: `Replay` is idempotent via `INSERT ... ON CONFLICT DO NOTHING` or by checking `dead` existence first; returning success if the row is already in `outbox` (i.e., previously replayed and not yet published) avoids duplicate `event_id` creation (PK).

- **Versioning**: `buf breaking` will flag the new service as non-breaking (adding a service); removing/reusing field numbers in `entities.proto` remains forbidden (`reserved`).

- **Reference**: `docs/review/arch-review-2026-08-fix-plan.md §W-J` (P1-13 dedup, P1-14 versioning, dead-letter gauge) and `internal/infra/events/outbox_worker.go:55,217-238` (dead-letter insert path).
