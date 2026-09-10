# Torchwood

**English** | [简体中文](README_ZH.md)

Torchwood is an Appwrite-inspired, **AI/Agent-Native** Backend-as-a-Service built with Go, PostgreSQL and gRPC/grpc-gateway. It provides auth, a dynamic document database, file storage, function execution and an Admin Console — APIs and tooling designed for LLM agents, automation and MCP from day one.

## Features

- **Agent-native API**: Protobuf is the single source of truth; `buf generate` produces gRPC stubs, grpc-gateway handlers and OpenAPI (`genproto/`). Scoped API Keys (`x-api-key`) expose the Server API for automation.
- **Projects**: multi-project isolation; each `(project.id, database.id)` maps to a Postgres schema.
- **Auth**: email/password, JWT access/refresh with rotation, session cookies, Email/Phone OTP, OAuth2 (Google/GitHub/WeChat), anonymous sessions, Magic URL, one-time JWT, TOTP MFA and email-change confirmation.
- **Database**: schema-per-database, `_tenant` isolation, `_acl` document permissions enforced by Postgres RLS policies, typed query AST (`pkg/query`; Appwrite-style DSL as client-side sugar), keyset pagination, bulk ops, increments and array updates.
- **Storage**: S3/MinIO-compatible, upload/download/view, thumbnails, public buckets, HMAC file tokens, chunked resumable upload.
- **Functions**: Docker build/run executor, sync/async execution, async `cmd/worker` and retention policy.
- **Console**: React SPA embedded in the Go binary at `/console/`.

## Tech Stack

### Backend

- Go 1.26.5 · [Lynx](https://github.com/lynx-go/lynx) (service framework) · gRPC + grpc-gateway · [Wire](https://github.com/google/wire) DI · [bun](https://github.com/uptrace/bun) ORM · PostgreSQL · Redis · MinIO/S3

### Frontend

- React 19 + TypeScript 6 · Vite 8 · React Router 7 · TanStack Query 5 · Tailwind CSS 3 + shadcn/ui · sonner · lucide-react

See `go.mod` and `console/package.json` for exact versions.

## Quick Start

### Prerequisites

Go 1.26.5+, Node.js 22+ with pnpm, Docker + Compose, [Task](https://taskfile.dev/) (`go install github.com/go-task/task/v3/cmd/task@latest`).

### 1. Start infrastructure

```bash
task docker:up
```

Starts PostgreSQL (5432), Redis (6379) and MinIO (9000/9001). Ports are overridable via `POSTGRES_PORT`/`REDIS_PORT`/`MINIO_API_PORT`/`MINIO_CONSOLE_PORT` in `.env`.

### 2. Configure

```bash
cp .env.example .env
```

Key vars (`TORCHWOOD_` prefix, see `internal/pkg/config/config.proto` + `internal/pkg/config/bind.go`):

```env
# Runtime DSN: non-superuser authenticator (see docs/developer/13-operations.md §4.5;
# run its bootstrap SQL against the compose DB before first server start)
TORCHWOOD_DATA_DATABASE_SOURCE=postgres://tw_authenticator:<password>@127.0.0.1:5432/torchwood?sslmode=disable
TORCHWOOD_SECURITY_JWT_SECRET=dev-only-0123456789abcdef-0123456789abcdef  # >=32 chars, no weak substrings
TORCHWOOD_SECURITY_SETUP_TOKEN=dev-setup-0123456789abcdef0123456789abcdef
TORCHWOOD_STORAGE_S3_ENDPOINT=http://127.0.0.1:9000
TORCHWOOD_STORAGE_S3_ACCESS_KEY_ID=minioadmin
TORCHWOOD_STORAGE_S3_SECRET_ACCESS_KEY=minioadmin
```

Generate strong random values for `JWT_SECRET`/`SETUP_TOKEN` in production (`openssl rand -hex 32`). Without `SETUP_TOKEN`, first-admin registration is rejected. The compose `POSTGRES_USER` is an initdb bootstrap (superuser) account: it belongs to the migrate/bootstrap side only, never the runtime config (dual-account contract, `docs/developer/13-operations.md` §4.5).

### 3. Migrate

```bash
task db:migrate   # run with the bootstrap (superuser) DSN — see §4.5
```

### 4. Toolchain & codegen

```bash
task tools:install   # buf, wire, migrate, protoc-gen-go, golangci-lint (first time)
task generate:all    # buf generate + config proto + wire:all
task console:install # pnpm install (first time)
```

### 5. Build & run

```bash
task build        # console:build then server + worker + CLI -> ./bin/
./bin/server      # or ./bin/server.exe on Windows
# dev mode:
task dev:server   # go run ./cmd/server
```

Modify Console code? `task console:build` before `task build` — the SPA is `//go:embed`'d.

### 6. Bootstrap

Open `http://127.0.0.1:9080/console/` — the login page becomes a setup form. Provide `project_id` + `database_id` and the `SETUP_TOKEN`:

- creates the owner admin (first admin is always `owner`);
- creates the project and its system `default` database (creates the extra `database_id` if not `default`).

Create API Keys afterwards in Console → **API Keys** and call the Server API with `x-api-key`.

### Endpoints (defaults from `configs/config.yaml.template`)

| Surface | Address |
|---------|---------|
| Console | `http://127.0.0.1:9080/console/` |
| HTTP / grpc-gateway | `http://127.0.0.1:9080/v1/...` |
| gRPC (loopback) | `127.0.0.1:9060` |
| Metrics | `http://127.0.0.1:9040/metrics` |
| Health | `http://127.0.0.1:9080/healthz/liveness`, `/healthz/readiness` |

## Common Tasks

| Task | Purpose |
|------|---------|
| `task docker:up` / `down` / `clean` | start / stop / wipe local infra |
| `task db:migrate` | run `db/migrations` |
| `task generate:proto` / `generate:config` / `wire:all` / `generate:all` | buf / config proto / Wire |
| `task console:install` / `console:build` / `console:dev` | frontend pnpm workflow |
| `task dev:server` / `task dev:worker` | run server/worker without building |
| `task build` | console + server + worker + `bin/torchwood` CLI |
| `task test` | SDK Go/TS tests + `go test -v ./... -cover` |
| `task lint` | `go vet` + `golangci-lint` + console lint |

## Project Structure

```
.
├── cmd/server/          # server entry + Wire (provides.go -> wire_gen.go)
├── cmd/worker/          # async worker (functions queue consumer, own Wire)
├── cmd/torchwood/          # Torchwood CLI (lynx-go/commands, sdk/go InvokeJSON)
├── console/             # Admin Console SPA (embed.go -> //go:embed dist)
├── configs/             # config.yaml.template (+ local config.yaml)
├── db/migrations/       # golang-migrate SQL
├── docker/local/        # Compose: Postgres + Redis + MinIO
├── genproto/            # generated protobuf (*.pb.go / *.pb.gw.go / *.swagger.json)
├── proto/               # protobuf sources (client / server / console / shared)
├── internal/
│   ├── api/             # transport: clientgrpc / consolegrpc / servergrpc / serverhttp
│   ├── app/             # use-cases (client / console / server / functions / storage)
│   ├── domain/          # models + ports (audit / auth / databases / functions / users / ...)
│   ├── infra/           # adapters (bun / documentdb / storage / queue / messaging / ...)
│   ├── pkg/             # in-process shared (config / database / contexts / buildinfo)
│   └── testutil/        # integration test helpers
├── pkg/                 # reusable libs (crud / query DSL / jwtparser / password / idgen / semaphore / secretbox)
├── sdk/                 # official SDKs: typescript/ + go/ + demo/
├── buf.yaml / buf.gen.yaml
├── Taskfile.yml
└── README.md / README_ZH.md
```

## Architecture

- **Clean Architecture**: `internal/api` (transport) → `internal/app` (use-cases) → `internal/domain` (models & ports) → `internal/infra` (adapters). `domain` holds interfaces, `infra` implements them.
- **Wire DI**: `cmd/server/provides.go` declares the provider set, `cmd/server/wire_gen.go` (and `cmd/worker` equivalent) is generated by `task wire:all`. Re-run after provider changes.
- **Three processes**: `server` (gRPC + gateway + custom HTTP handlers + metrics + embedded Console), `worker` (function execution queue consumer, independent Wire), `CLI` (`bin/torchwood`, lynx-go/commands over `sdk/go/server` `InvokeJSON`; no direct `genproto` import — `rpc` escape hatch covers all new RPCs).
- **Three-tier storage**: `public` control plane & event spine (`projects`, `admins`, `api_keys`, `audit_logs`, `outbox`/`outbox_dead`, global catalog tables `catalog_databases`/`catalog_collections`, via bun + golang-migrate); `tw_<project.id>` project plane — system static tables (`users`, `sessions`, `identities`, `groups`, `memberships`, `buckets`, `files`) + ledger/functions/OAuth (`internal/infra/projectschema/`); `tw_<project.id>_<database.id>` business plane — user collections only (real tables named by collectionID, `_tenant` + embedded `_acl` + RLS policies).
- **API surface**: Protobuf is the single source of truth (`proto/` → `genproto/`). REST via grpc-gateway; file multipart and OAuth callbacks via `internal/api/serverhttp`. gRPC methods require `method_auth` annotations (collected into a policy registry at startup).
- **Auth**: end-user JWT/session cookie, API Key (participates as `keys` + `key:<id>` roles in `_acl`, not a bypass), console admin JWT (`TORCHWOOD_session_console` HttpOnly cookie). Admins target a project via `X-Torchwood-Project`.
- **Hardening (recent, not expanded)**: outbox dead-letter replay `torchwood admin outbox list-dead/replay` (`document_events_outbox_dead`); global Redis semaphore `pkg/semaphore` (build 4 / run 16, TTL-guarded, in-memory fallback); per-statement 5s/10s `context.WithTimeout` across stores; `golangci-lint` 0 warnings full and `--new-from-rev=origin/main` ratchet; `buf breaking --against '.git#branch=origin/main'` gate.

## Testing

```bash
task test   # lint:go + sdk/go + sdk/typescript + go test -v ./... -cover
```

Integration tests (`internal/infra/documentdb/postgres_test.go`, `internal/app/client/account_test.go`, etc.) auto-create/drop the `TORCHWOOD_test` DB. DSNs come from `TORCHWOOD_TEST_DATABASE_SOURCE` / `TORCHWOOD_TEST_ADMIN_DATABASE_SOURCE` (in `.env.example`); `task test` loads `.env` automatically.

## SDKs

See [`sdk/README.md`](sdk/README.md):

- **TypeScript** (`sdk/typescript`, `@torchwood/sdk`) — Client + Server API over HTTP (grpc-gateway), with `sdk/demo`.
- **Go** (`sdk/go`, `github.com/torchwoodcloud/torchwood/sdk/go`) — gRPC-direct thin wrappers: `client` (end-user auth, auto token refresh) and `server` (API Key + `InvokeJSON` dynamic dispatch; CLI is built on it).

```bash
task sdk:install && task sdk:build
task sdk:demo   # http://localhost:5174
```

## Developer Documentation

Full docs live in [`docs/developer/`](docs/developer/README.md):

| Doc | Scope |
|-----|-------|
| [01-overview](docs/developer/01-overview.md) | architecture, stack, layers, call chain |
| [02-quickstart](docs/developer/02-quickstart.md) | setup, bootstrap, endpoints, CLI |
| [03-configuration](docs/developer/03-configuration.md) | config.proto, `TORCHWOOD_` env mapping |
| [04-codegen](docs/developer/04-codegen.md) | Task / Buf / Wire |
| [05-authentication](docs/developer/05-authentication.md) | JWT / session / API Key / scopes |
| [06-databases](docs/developer/06-databases.md) | dynamic documents, `_acl` + RLS permissions, query AST |
| [07-storage](docs/developer/07-storage.md) | S3/MinIO, chunked upload, file tokens |
| [08-functions](docs/developer/08-functions.md) | Docker executor, worker, lifecycle |
| [09-api-guide](docs/developer/09-api-guide.md) | adding gRPC methods, errors, pagination |
| [10-console](docs/developer/10-console.md) | frontend structure, session cookie |
| [11-testing](docs/developer/11-testing.md) | test layers, CI, lint, observability |
| [12-sdk](docs/developer/12-sdk.md) | SDK guide |
| [13-operations](docs/developer/13-operations.md) | deploy, health, backup |
| [14-agent-tools](docs/developer/14-agent-tools.md) | agent tool overlay (18 verbs) |

Also: `AGENTS.md` (contributor conventions), `docs/roadmap.md` (AI/Agent-Native strategy), `docs/tech-decision.md`.

## License

MIT (TBD)
