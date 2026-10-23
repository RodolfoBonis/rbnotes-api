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
