# Mudanças de contrato (rbnotes-api)

A fonte da verdade é `api/openapi.yaml`. O resumo do desktop é
`rbnotes-desktop/docs/backend-contract.md`. Os dois são mantidos em sincronia; este arquivo
registra as decisões que ajustaram/esclareceram o contrato v1 e o porquê.

## v1 — implementação inicial (2026-10-23)

1. **Base e auth** — `/v1`, Bearer JWT do realm `rbnotes` (audience `rbnotes-api`). Sem mudança;
   só confirmação. O usuário nasce no primeiro request (sem convite/dono: app pessoal).

2. **Toda IA é assíncrona (202 + SSE), inclusive `GET /ai/diary/prompt`.**
   O contrato descrevia `GET /ai/diary/prompt?date= → { prompt }`, o que sugeria resposta
   síncrona. Porém a API **não executa** a CLI do Claude (quem executa é o ai-worker), então todo
   pedido de IA retorna `202 { id, status: "queued" }` e o resultado vem por
   `GET /ai/streams/{id}`. O disparador diário segue o mesmo fluxo dos demais.

3. **Resultado estruturado via eventos `json` do SSE.** Todas as execuções usam saída estruturada
   da CLI (`--json-schema`). O SSE emite o JSON em `json` (parciais) e encerra com `done`; o objeto
   final (ex.: `{ text, cites }` do chat, `{ text, sources }` do ask) é persistido. O "texto em
   streaming" do chat/ask é o campo `text` desse objeto, que cresce ao longo dos eventos `json`. O
   tipo de evento `text` fica reservado (não usado no v1), para não haver execução mista
   texto+estruturado (a CLI faz uma coisa ou outra, não as duas).

4. **`note_chat` aceita `transcript: Segment[]` opcional (aditivo).** Para honrar "citações `atMs`
   só de trechos reais", o servidor ancora as citações aos `startMs` reais quando o cliente envia a
   transcrição no corpo. Sem `transcript`, as citações não são filtradas (o prompt ainda instrui a
   não inventar). O `threadId` é opcional e, quando ausente, assume o id da nota (uma conversa por
   nota), com workdir estável `chat/<threadId>` para o `--resume`.

5. **`ask` — fontes restritas às candidatas.** O servidor seleciona as candidatas (FTS pt-BR com
   `unaccent` sobre título+tldr+transcrição, completando com recência; máx. ~12) e só aceita em
   `sources` os `noteId` dentre elas; o `title` devolvido é sempre o canônico do servidor.

6. **`diary_month` — contexto por notas indexadas recentes.** O resumo mensal usa o pool de notas
   do índice de busca do usuário como contexto (limitação conhecida: o índice usa a data de
   indexação no servidor, não a data da nota no cliente). Evolução futura: receber as notas do mês
   no corpo ou indexar a data de criação da nota.

7. **Áudio** — `PUT /notes/{id}/audio?part=&total=` com `application/octet-stream`; resposta
   `{ part, completed }` (completed=true na última parte). Chunk ≤ 50 MiB. `409` se uma parte > 1
   chegar sem o upload ter sido iniciado (envie a parte 1 primeiro). O id da nota é restrito a
   `[A-Za-z0-9_-]` (barra path traversal na chave do objeto).

8. **Cursor do pull** é um inteiro em string, monotônico e global por usuário (sequência
   `change_seq` alocada no push). Vazio/`0` = do início.

9. **Modelos** — whitelist `sonnet | opus | haiku` no campo `model` dos pedidos; fora disso → `422`.
   Cota diária por usuário via `AI_DAILY_LIMIT` → `429` com `code: ai_daily_limit`.

10. **Allowlist de e-mail (`AUTH_ALLOWED_EMAILS`) — `403 not_allowed`.** O Keycloak cria o usuário
    no primeiro login brokered (qualquer conta Google), então um token válido do realm não basta.
    Com a env preenchida (lista separada por vírgula, case-insensitive), todo endpoint autenticado
    exige `email_verified=true` e e-mail na lista; caso contrário responde `403` problem+json com
    `code: "not_allowed"` e NÃO cria o registro em `users`. Em `APP_ENV=prod` com `AUTH_MODE=oidc`
    a API recusa subir sem a lista. Aditivo: nova resposta `403` documentada em todos os endpoints
    autenticados.

## v1 — correção do stream de IA e do sync de note_ai (2026-10-23)

11. **Evento `done` do SSE carrega o resultado canônico.** Antes o `done` ia com `data` vazio
    (tanto no fechamento normal quanto no `streamFinalState`, quando a stream do Redis já havia
    expirado). O resultado só existia em `ai_jobs.result` e, para entidades, no pull. Clientes que
    tratavam os eventos `json` (ou o envelope) como o objeto final acabavam com um resultado vazio.
    Agora o `done` leva o **resultado validado completo** num único evento (o mesmo JSON de
    `ai_jobs.result`); os `json` parciais seguem existindo só para a UI de streaming. **Envelope:**
    todo evento SSE tem `data:` = `{"data":"<conteúdo>","code"?:"<código>"}`; o cliente desembrulha
    e só então usa o conteúdo. Aditivo (sem quebra para quem já desembrulhava e lia o `done`).

12. **`POST /sync/push` recusa `note_ai` vivo sem conteúdo → `422 invalid_note_ai`.** Um `note_ai`
    não-tombstone precisa ter `title` e `tldr` não vazios. Aceita as duas formas do contrato: o JSON
    "bare" do enrich do servidor (`{"title":...,"tldr":...}`) e a do desktop (`{"noteId":...,`
    `"aiJson":"{...}"}`). Protege o resumo correto do servidor de ser sobrescrito por LWW por um
    cliente antigo que gravou `note_ai` vazio (bug do envelope SSE). Tombstones (`deleted:true`) são
    sempre aceitos, inclusive com `data` vazio — usados para purgar dados de demo que vazaram.

## Fase 3 — consumo de IA, áudio, notas cifradas e resumo semanal (2026-10-24)

13. **Consumo de IA registrado por pedido + `GET /ai/usage`.** O `ai_jobs` passou a guardar o
    consumo informado pela CLI do Claude Code na linha `result` (migração `00008_ai_usage`):
    `cost_usd` (estimativa em USD — o dono usa assinatura), `input/output/cache_creation/
    cache_read_tokens`, `duration_ms`, `num_turns` e `resolved_model` (ex.: `claude-haiku-4-5-…`).
    O worker acumula via `SetAIJobUsage` em todas as execuções (inclui o retry sem `--resume` do
    chat). Novo `GET /ai/usage?from=&to=&tz=`: datas inclusivas (YYYY-MM-DD) no fuso IANA (padrão
    `UTC`), padrão últimos 30 dias, intervalo máximo 366 dias, escopo por usuário; devolve
    `totals`, série `days[]` por kind (no fuso), `byKind[]` e `quota{limit,used24h}`;
    `costIsEstimate` é sempre `true`. Métricas novas `ai_cost_usd_total{kind}` e
    `ai_tokens_total{kind,type}`.

14. **`DELETE /notes/{id}/audio` → 204, idempotente.** Remove o áudio da nota e aborta o multipart
    em andamento; responde `204` mesmo se objeto/upload não existirem. Escopo por usuário; id
    restrito a `[A-Za-z0-9_-]`. Suporta o fluxo de "apagar áudio"/"apagar todos os dados" do app.

15. **Notas cifradas (bloqueadas).** No sync, quando o `data` de `note`/`segment_set`/`note_ai`/
    `chat_message` chega como `{ "enc":"v1", "nonce", "ct" }`: o servidor guarda como está, **não
    indexa no FTS** (e remove o índice anterior da nota, tirando-a de `ask`/`diary_month`/
    `weekly_summary`) e **aceita o `note_ai` cifrado sem exigir title/tldr** (a regra
    `invalid_note_ai` não vale para cifrados). Nos endpoints de IA, `enrich` e `chat` aceitam
    `"ephemeral": true`: o worker roda normalmente mas **não grava a entidade em texto claro nem
    indexa**; o resultado volta só no `done` do SSE e o `ai_jobs.input`/`result` é limpo (`{}`) ao
    fim. O consumo (custo/tokens) continua registrado (não é texto claro).

16. **Resumo semanal — kind `weekly_summary` + `POST /ai/weekly-summary`.** Migração
    `00009_weekly_summary` estende o CHECK de `ai_jobs.kind`. O endpoint recebe `{ weekStart, tz }`
    (weekStart YYYY-MM-DD; tz IANA, padrão UTC) e responde `202` + SSE. O prompt (pt-BR) usa as
    notas recentes do índice como contexto (as cifradas ficam de fora, pois não entram no
    `note_search`), com saída estruturada estrita (`WeeklySummary`): `title`, `summary`,
    `highlights`, `openTasks[]` (com `noteId` restrito às notas conhecidas) e `themes[]`.
