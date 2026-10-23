# rbnotes-api

Backend do **RbNotes** — app desktop pessoal de notas de voz. A transcrição é feita localmente no
Mac; o backend recebe **só texto**. Responsabilidades:

- **Sync offline-first** (LWW por `updatedAt`, cursor monotônico por usuário).
- **Áudio** das notas em chunks → MinIO interno (multipart S3), download por streaming.
- **IA** assíncrona via **Claude Code headless** (enriquecimento, chat, ask, regra de coleção,
  diário mensal, disparador diário), com resposta em tempo real por **SSE**.
- **Auth** OIDC (Keycloak realm `rbnotes`), tudo escopo por usuário.

Contrato: a fonte da verdade é [`api/openapi.yaml`](api/openapi.yaml) (lint no CI). Ajustes de
contrato ficam em [`api/CHANGES.md`](api/CHANGES.md).

## Arquitetura

```
desktop ──HTTPS──> api (cmd/api)            ──> Postgres (sync, filas, ai_jobs, FTS)
                     │                       ──> MinIO    (áudio: users/<uid>/notes/<id>.m4a)
                     │  enfileira ai_job          │
                     │  SSE <── Redis Streams <────┤
                     ▼                             │
              jobs (Postgres, SKIP LOCKED)         │
                     ▲                             │
                     │ claim 1 por vez             │
             ai-worker (cmd/ai-worker) ── CLI `claude` headless (Node) ──> Anthropic
```

- **`cmd/api`** — HTTP público (`HTTP_ADDR`, ex. `:8080`) só com a aplicação; listener **interno**
  separado (`METRICS_ADDR`, ex. `:9090`) com `/metrics` + `/healthz`. O `/metrics` **nunca** é
  exposto no host público. Aplica as migrations no startup.
- **`cmd/ai-worker`** — consome os jobs `ai_job` (um por vez), roda a CLI do Claude Code headless e
  transmite a resposta por Redis Streams. Imagem própria (`node:22-slim` + `@anthropic-ai/claude-code`).
- **Fila** (`internal/platform/queue`) — Postgres com `FOR UPDATE SKIP LOCKED`, lease + lease_token,
  dedupe, backoff com jitter. Rate limit da assinatura = **retry**; timeout de 6 min → permanente;
  tentativas esgotadas por erro transitório fecham a stream com erro (`giveUp`).
- **IA** (`internal/ai`) — toda saída é **estruturada** (JSON Schema para a CLI) e **revalidada**
  no Go (DisallowUnknownFields, required, ranges) antes de persistir. Schemas em `schemas.go`.
- **Sync** (`internal/sync`) — tabelas por entidade (`notes`, `segment_sets`, `folders`, `actions`,
  `collections`, `vocabulary`, `chat_messages`, `note_ai`); push LWW numa transação (o lock de
  `sync_cursors` serializa e garante `change_seq` crescente); pull por `UNION ALL` ordenado.
- **Busca (`ask`)** — índice FTS em português com `unaccent` (`note_search.tsv`, coluna gerada)
  sobre título + tldr + transcrição; candidatas por relevância + recência (máx. ~12).
- **Observabilidade** — métricas Prometheus (`ai_jobs_total{kind,result}`, `http_*`, `jobs_queue`),
  logs slog JSON com redação de segredos, erros `application/problem+json` com `code`.

## Rodando local

Pré-requisitos: Go 1.26, Docker/Colima (testes de integração), `claude` CLI logado (só para o
ai-worker), Node 22 (lint do OpenAPI).

```bash
cp .env.example .env            # AUTH_MODE=dev já vem configurado p/ dev local
make db-up                      # Postgres 17 + Redis + MinIO (docker compose)
make run                        # API em :8080 (migrations no startup)

# ai-worker (opcional; precisa do token do Claude):
export CLAUDE_CODE_OAUTH_TOKEN=... ; make run-ai-worker
```

No modo dev (`AUTH_MODE=dev`, só com `APP_ENV=dev` e `AUTH_ISSUER` vazio) a identidade vem do
header `X-Debug-User` (e opcionalmente `X-Debug-Email`, `X-Debug-Name`):

```bash
curl -s localhost:8080/v1/me -H 'X-Debug-User: eu' -H 'X-Debug-Email: eu@x'
```

### Codegen, testes, lint

```bash
make gen          # sqlc generate (db/gen)
make test         # go test -race ./... (integração via testcontainers/Colima)
make test-short   # só unit (sem Docker)
make lint         # golangci-lint
make openapi-lint # Redocly
make smoke        # smoke manual do note_enrich contra a CLI real (precisa de `claude` logado)
```

Os testes de integração usam `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock` (Colima).
O smoke manual (`internal/ai/smoke_manual_test.go`, build tag `manual`) **não** roda no CI; use-o
localmente com o `claude` logado:

```bash
CLAUDE_CODE_OAUTH_TOKEN=$(cat token) AI_MODEL=sonnet make smoke
```

## Variáveis de ambiente

Ver `.env.example`. As principais:

| Grupo | Variável | Notas |
|------|----------|-------|
| HTTP | `HTTP_ADDR` | público (app) |
| Métricas | `METRICS_ADDR` | **interno** (/metrics + /healthz) |
| DB | `DATABASE_URL` | obrigatório |
| Redis | `REDIS_ADDR` | canal do SSE |
| Auth | `AUTH_MODE`/`AUTH_ISSUER`/`AUTH_AUDIENCE` | oidc em prod; `rbnotes` / `rbnotes-api` |
| MinIO | `MINIO_ENDPOINT`/`MINIO_ACCESS_KEY`/`MINIO_SECRET_KEY`/`MINIO_BUCKET` | SEGREDO (creds) |
| Áudio | `AUDIO_CHUNK_LIMIT` | 50 MiB default |
| IA | `CLAUDE_CODE_OAUTH_TOKEN` | **SEGREDO**, só o ai-worker |
| IA | `AI_MODEL` | `sonnet` (whitelist: sonnet/opus/haiku) |
| IA | `AI_DAILY_LIMIT` | pedidos/usuário em 24 h (0 = sem teto) |
| IA | `AI_JOB_TIMEOUT` | 6 min |
| IA | `AI_HOME`/`AI_WORKDIR` | `~/.claude` guarda as sessões do `--resume` do chat |

## Token do Claude (gerar/rotacionar)

O ai-worker autentica a CLI com `CLAUDE_CODE_OAUTH_TOKEN` (token da assinatura). Para gerar:

```bash
claude setup-token          # faz login e imprime o token
```

Guarde no Vault e deixe o External/Sealed Secret montar no pod:

```
k3s/rbnotes/prod/claude   ->  CLAUDE_CODE_OAUTH_TOKEN
```

Rotação: rode `claude setup-token` de novo, atualize `k3s/rbnotes/prod/claude` no Vault e reinicie o
deployment do ai-worker. O token **nunca** é logado (vai só no ambiente do processo filho da CLI).

## Deploy (runbook)

CI (push no `main`): `lint` + `codegen` (sqlc diff + OpenAPI lint) + `test -race` com testcontainers.

CD (após CI verde, `.github/workflows/cd.yaml`):

1. Bump de `version.txt` (`feat:` = minor; senão patch).
2. Build/push de duas imagens no ECR: `rodolfobonis/rbnotes-api` (Dockerfile distroless) e
   `rodolfobonis/rbnotes-ai-worker` (Dockerfile.ai-worker, `node:22-slim` + a última CLI do npm).
3. Bump das tags em `RodolfoBonis/k3s-apps/apps/rbnotes/values-api-prod.yaml`
   (`web-server.image.tag` e o worker `ai-worker`).
4. `argocd app sync rbnotes-api-prod`.
5. Release no GitHub + aviso no Telegram.

Secrets do GitHub necessários: `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_REGION`,
`GH_TOKEN` (push no k3s-apps), `ARGOCD_SERVER`, `ARGOCD_TOKEN`, `BOTTOKEN`, `CHAT_ID`.

### O que a infra (k3s-apps) precisa

`apps/rbnotes/values-api-prod.yaml` deve ter:

- **api** (`web-server`): deployment com as imagens do ECR; `HTTP_ADDR=:8080`, `METRICS_ADDR=:9090`
  (service só interno p/ o Prometheus — **não** publicar `/metrics` no Ingress público); Ingress do
  host `api.notes.rodolfodebonis.com.br` apontando só para `:8080`.
- **ai-worker** (worker `name: ai-worker`, imagem `rbnotes-ai-worker`): 1 réplica (um job por vez),
  volume persistente em `/home/node/.claude` (sessões do `--resume`) e `/home/node/work`.
- **envs comuns**: `DATABASE_URL`, `REDIS_ADDR`, `AUTH_ISSUER=https://auth.rodolfodebonis.com.br/realms/rbnotes`,
  `AUTH_AUDIENCE=rbnotes-api`, `MINIO_ENDPOINT` (service interno do MinIO), `MINIO_BUCKET=rbnotes-audio`,
  `CORS_ALLOWED_ORIGINS`.
- **secrets** (Sealed/External Secret a partir do Vault):
  - `k3s/rbnotes/prod/claude` → `CLAUDE_CODE_OAUTH_TOKEN` (só no ai-worker).
  - `k3s/rbnotes/prod/minio` → `MINIO_ACCESS_KEY`, `MINIO_SECRET_KEY`.
  - `k3s/rbnotes/prod/db` → `DATABASE_URL` (com senha).
- **Keycloak**: realm `rbnotes`, client público `rbnotes-desktop` (PKCE, redirect loopback) e o
  client/escopo que injeta a audience `rbnotes-api` nos tokens.
- **MinIO**: bucket `rbnotes-audio` (a API cria se não existir, dado permissão).
- **ArgoCD**: Application `rbnotes-api-prod`.
```
