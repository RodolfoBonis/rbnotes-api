// Package sync implementa a sincronização offline-first entre o app desktop e o servidor.
//
// Modelo: cada entidade (note, segment_set, folder, action, collection, vocabulary, chat_message,
// note_ai) tem sua tabela, todas com o mesmo formato (id do cliente, updated_at do cliente,
// deleted, data e change_seq). O push aplica LWW por updated_at — se o servidor já tem uma versão
// mais nova, a mudança vira conflito e a versão do servidor volta ao cliente. O pull devolve tudo
// o que mudou depois do cursor (change_seq monotônico global por usuário), em ordem.
package sync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	dbgen "github.com/RodolfoBonis/rbnotes-api/db/gen"
)

// Entidades sincronizáveis. O servidor nunca mistura dados entre usuários: tudo é escopo por
// user_id (PK (user_id, id) em cada tabela).
const (
	EntityNote        = "note"
	EntitySegmentSet  = "segment_set"
	EntityFolder      = "folder"
	EntityAction      = "action"
	EntityCollection  = "collection"
	EntityVocabulary  = "vocabulary"
	EntityChatMessage = "chat_message"
	EntityNoteAi      = "note_ai"
)

var validEntities = map[string]bool{
	EntityNote: true, EntitySegmentSet: true, EntityFolder: true, EntityAction: true,
	EntityCollection: true, EntityVocabulary: true, EntityChatMessage: true, EntityNoteAi: true,
}

// MaxPushChanges limita quantas mudanças um push pode trazer (defesa contra payloads enormes).
const MaxPushChanges = 1000

// pullPageSize é o teto de mudanças por página do pull (o cliente pagina pelo cursor).
const pullPageSize = 500

// maxClockSkew limita o quanto o updatedAt do cliente pode estar no futuro: um relógio adiantado
// não pode "vencer" o LWW para sempre. Acima disso, o updatedAt é fixado em now() (com log).
const maxClockSkew = 5 * time.Minute

// ErrTooManyChanges indica um push acima de MaxPushChanges.
var ErrTooManyChanges = fmt.Errorf("sync: push com mais de %d mudanças", MaxPushChanges)

// maxEntityIDLen limita o id vindo do cliente (defesa; ids são ULIDs/UUIDs curtos).
const maxEntityIDLen = 256

// Erros de validação do push/pull (viram 422/400 na borda).
var (
	// ErrUnknownEntity: entidade fora do conjunto sincronizável.
	ErrUnknownEntity = errors.New("sync: entidade desconhecida")
	// ErrInvalidID: id de entidade vazio ou longo demais.
	ErrInvalidID = errors.New("sync: id inválido")
	// ErrBadCursor: parâmetro since/cursor malformado.
	ErrBadCursor = errors.New("sync: cursor inválido")
)

// Change é uma mudança de uma entidade (contrato camelCase com o desktop).
type Change struct {
	Entity    string          `json:"entity"`
	ID        string          `json:"id"`
	UpdatedAt time.Time       `json:"updatedAt"`
	Deleted   bool            `json:"deleted"`
	Data      json.RawMessage `json:"data"`
}

// PushResult é a resposta do push: ids aplicados e conflitos (com a versão do servidor).
type PushResult struct {
	Applied   []string `json:"applied"`
	Conflicts []Change `json:"conflicts"`
}

// PullResult é a resposta do pull: a página de mudanças e o novo cursor.
type PullResult struct {
	Changes []Change `json:"changes"`
	Cursor  string   `json:"cursor"`
}

// Service coordena push/pull. Depende só das queries geradas e do pool (para transação).
type Service struct {
	q    *dbgen.Queries
	pool *pgxpool.Pool
}

// New constrói o serviço de sync.
func New(q *dbgen.Queries, pool *pgxpool.Pool) *Service {
	return &Service{q: q, pool: pool}
}

// Push aplica as mudanças do cliente numa única transação (o lock do sync_cursors serializa pushes
// concorrentes do mesmo usuário, mantendo o change_seq estritamente crescente).
func (s *Service) Push(ctx context.Context, userID pgtype.UUID, changes []Change) (PushResult, error) {
	if len(changes) > MaxPushChanges {
		return PushResult{}, ErrTooManyChanges
	}
	res := PushResult{Applied: []string{}, Conflicts: []Change{}}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return res, fmt.Errorf("sync: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	for _, ch := range changes {
		if !validEntities[ch.Entity] {
			return res, fmt.Errorf("%w: %q", ErrUnknownEntity, ch.Entity)
		}
		if ch.ID == "" || len(ch.ID) > maxEntityIDLen {
			return res, fmt.Errorf("%w: %q", ErrInvalidID, ch.ID)
		}
		// Relógio do cliente adiantado demais: limita o updatedAt a now()+skew para não vencer o
		// LWW para sempre.
		if ch.UpdatedAt.After(time.Now().Add(maxClockSkew)) {
			slog.WarnContext(ctx, "sync: updatedAt no futuro, limitado a now()",
				slog.String("entity", ch.Entity), slog.String("id", ch.ID),
				slog.Time("updated_at", ch.UpdatedAt))
			ch.UpdatedAt = time.Now()
		}
		server, found, err := getEntity(ctx, q, ch.Entity, userID, ch.ID)
		if err != nil {
			return res, err
		}
		if found {
			serverTime := server.UpdatedAt.Time
			// O JSONB volta normalizado pelo Postgres ({"a": 1}); o do cliente vem cru ({"a":1}).
			// Canoniza os dois para o desempate ser determinístico entre dispositivos.
			cData, sData := rawOrEmpty(ch.Data), rawOrEmpty(server.Data)
			cCmp, sCmp := canonicalJSON(cData), canonicalJSON(sData)
			switch {
			case ch.UpdatedAt.Before(serverTime):
				// O servidor tem uma versão mais nova: conflito, devolve a do servidor.
				res.Conflicts = append(res.Conflicts, Change{
					Entity: ch.Entity, ID: ch.ID, UpdatedAt: serverTime,
					Deleted: server.Deleted, Data: sData,
				})
				continue
			case ch.UpdatedAt.Equal(serverTime):
				// Empate de timestamp: idempotente se o conteúdo é igual; senão desempata pelo
				// conteúdo (comparação determinística e simétrica entre dispositivos).
				if ch.Deleted == server.Deleted && bytes.Equal(cCmp, sCmp) {
					res.Applied = append(res.Applied, ch.ID)
					continue
				}
				if bytes.Compare(cCmp, sCmp) <= 0 {
					res.Conflicts = append(res.Conflicts, Change{
						Entity: ch.Entity, ID: ch.ID, UpdatedAt: serverTime,
						Deleted: server.Deleted, Data: sData,
					})
					continue
				}
				// Incoming vence o desempate: segue para o upsert abaixo.
			}
		}
		seq, err := q.NextChangeSeq(ctx, userID)
		if err != nil {
			return res, fmt.Errorf("sync: alocar change_seq: %w", err)
		}
		if err := upsertEntity(ctx, q, ch.Entity, userID, ch, seq); err != nil {
			return res, err
		}
		if err := s.indexNote(ctx, q, ch.Entity, userID, ch); err != nil {
			return res, err
		}
		res.Applied = append(res.Applied, ch.ID)
	}
	if err := tx.Commit(ctx); err != nil {
		return res, fmt.Errorf("sync: commit: %w", err)
	}
	return res, nil
}

// indexNote mantém o índice FTS (note_search) a partir do push de notas: grava o título (do data da
// nota) ou remove o índice quando a nota é apagada. O tldr/transcrição vêm do enrich (ver ai).
func (s *Service) indexNote(ctx context.Context, q *dbgen.Queries, entity string, userID pgtype.UUID, ch Change) error {
	if entity != EntityNote {
		return nil
	}
	if ch.Deleted {
		return q.DeleteNoteSearch(ctx, dbgen.DeleteNoteSearchParams{UserID: userID, NoteID: ch.ID})
	}
	var payload struct {
		Title string `json:"title"`
	}
	_ = json.Unmarshal(ch.Data, &payload)
	if payload.Title == "" {
		return nil
	}
	return q.UpsertNoteSearchTitle(ctx, dbgen.UpsertNoteSearchTitleParams{UserID: userID, NoteID: ch.ID, Title: payload.Title})
}

// Pull devolve a página de mudanças após o cursor e o novo cursor (o maior change_seq devolvido).
func (s *Service) Pull(ctx context.Context, userID pgtype.UUID, cursor string) (PullResult, error) {
	cur, err := parseCursor(cursor)
	if err != nil {
		return PullResult{}, err
	}
	rows, err := s.q.PullChanges(ctx, dbgen.PullChangesParams{Uid: userID, Cur: cur, Lim: pullPageSize})
	if err != nil {
		return PullResult{}, fmt.Errorf("sync: pull: %w", err)
	}
	out := PullResult{Changes: make([]Change, 0, len(rows)), Cursor: strconv.FormatInt(cur, 10)}
	for _, r := range rows {
		out.Changes = append(out.Changes, Change{
			Entity: r.Entity, ID: r.ID, UpdatedAt: r.UpdatedAt.Time,
			Deleted: r.Deleted, Data: rawOrEmpty(r.Data),
		})
		if r.ChangeSeq > cur {
			cur = r.ChangeSeq
		}
	}
	out.Cursor = strconv.FormatInt(cur, 10)
	return out, nil
}

// parseCursor converte o since/cursor. Vazio = 0 (do início); malformado ou negativo = ErrBadCursor
// (400 na borda, em vez de silenciosamente recomeçar do zero e re-enviar tudo).
func parseCursor(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, ErrBadCursor
	}
	return n, nil
}

func rawOrEmpty(b []byte) json.RawMessage {
	if len(b) == 0 {
		return json.RawMessage("{}")
	}
	return b
}

// entityRow é a forma comum devolvida pelos getters de entidade.
type entityRow struct {
	UpdatedAt pgtype.Timestamptz
	Deleted   bool
	Data      []byte
}

// getEntity lê a versão atual de uma entidade (found=false quando não existe).
func getEntity(ctx context.Context, q *dbgen.Queries, entity string, userID pgtype.UUID, id string) (entityRow, bool, error) {
	var row entityRow
	var err error
	switch entity {
	case EntityNote:
		var r dbgen.GetNoteRow
		r, err = q.GetNote(ctx, dbgen.GetNoteParams{UserID: userID, ID: id})
		row = entityRow{r.UpdatedAt, r.Deleted, r.Data}
	case EntitySegmentSet:
		var r dbgen.GetSegmentSetRow
		r, err = q.GetSegmentSet(ctx, dbgen.GetSegmentSetParams{UserID: userID, ID: id})
		row = entityRow{r.UpdatedAt, r.Deleted, r.Data}
	case EntityFolder:
		var r dbgen.GetFolderRow
		r, err = q.GetFolder(ctx, dbgen.GetFolderParams{UserID: userID, ID: id})
		row = entityRow{r.UpdatedAt, r.Deleted, r.Data}
	case EntityAction:
		var r dbgen.GetActionRow
		r, err = q.GetAction(ctx, dbgen.GetActionParams{UserID: userID, ID: id})
		row = entityRow{r.UpdatedAt, r.Deleted, r.Data}
	case EntityCollection:
		var r dbgen.GetCollectionRow
		r, err = q.GetCollection(ctx, dbgen.GetCollectionParams{UserID: userID, ID: id})
		row = entityRow{r.UpdatedAt, r.Deleted, r.Data}
	case EntityVocabulary:
		var r dbgen.GetVocabularyRow
		r, err = q.GetVocabulary(ctx, dbgen.GetVocabularyParams{UserID: userID, ID: id})
		row = entityRow{r.UpdatedAt, r.Deleted, r.Data}
	case EntityChatMessage:
		var r dbgen.GetChatMessageRow
		r, err = q.GetChatMessage(ctx, dbgen.GetChatMessageParams{UserID: userID, ID: id})
		row = entityRow{r.UpdatedAt, r.Deleted, r.Data}
	case EntityNoteAi:
		var r dbgen.GetNoteAiRow
		r, err = q.GetNoteAi(ctx, dbgen.GetNoteAiParams{UserID: userID, ID: id})
		row = entityRow{r.UpdatedAt, r.Deleted, r.Data}
	default:
		return row, false, fmt.Errorf("sync: entidade desconhecida %q", entity)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return entityRow{}, false, nil
	}
	if err != nil {
		return entityRow{}, false, fmt.Errorf("sync: get %s: %w", entity, err)
	}
	return row, true, nil
}

// upsertEntity grava a mudança (insere ou substitui, já decidido o LWW pelo chamador).
func upsertEntity(ctx context.Context, q *dbgen.Queries, entity string, userID pgtype.UUID, ch Change, seq int64) error {
	ts := pgtype.Timestamptz{Time: ch.UpdatedAt, Valid: true}
	data := rawOrEmpty(ch.Data)
	var err error
	switch entity {
	case EntityNote:
		err = q.UpsertNote(ctx, dbgen.UpsertNoteParams{UserID: userID, ID: ch.ID, UpdatedAt: ts, Deleted: ch.Deleted, Data: data, ChangeSeq: seq})
	case EntitySegmentSet:
		err = q.UpsertSegmentSet(ctx, dbgen.UpsertSegmentSetParams{UserID: userID, ID: ch.ID, UpdatedAt: ts, Deleted: ch.Deleted, Data: data, ChangeSeq: seq})
	case EntityFolder:
		err = q.UpsertFolder(ctx, dbgen.UpsertFolderParams{UserID: userID, ID: ch.ID, UpdatedAt: ts, Deleted: ch.Deleted, Data: data, ChangeSeq: seq})
	case EntityAction:
		err = q.UpsertAction(ctx, dbgen.UpsertActionParams{UserID: userID, ID: ch.ID, UpdatedAt: ts, Deleted: ch.Deleted, Data: data, ChangeSeq: seq})
	case EntityCollection:
		err = q.UpsertCollection(ctx, dbgen.UpsertCollectionParams{UserID: userID, ID: ch.ID, UpdatedAt: ts, Deleted: ch.Deleted, Data: data, ChangeSeq: seq})
	case EntityVocabulary:
		err = q.UpsertVocabulary(ctx, dbgen.UpsertVocabularyParams{UserID: userID, ID: ch.ID, UpdatedAt: ts, Deleted: ch.Deleted, Data: data, ChangeSeq: seq})
	case EntityChatMessage:
		err = q.UpsertChatMessage(ctx, dbgen.UpsertChatMessageParams{UserID: userID, ID: ch.ID, UpdatedAt: ts, Deleted: ch.Deleted, Data: data, ChangeSeq: seq})
	case EntityNoteAi:
		err = q.UpsertNoteAi(ctx, dbgen.UpsertNoteAiParams{UserID: userID, ID: ch.ID, UpdatedAt: ts, Deleted: ch.Deleted, Data: data, ChangeSeq: seq})
	default:
		return fmt.Errorf("sync: entidade desconhecida %q", entity)
	}
	if err != nil {
		return fmt.Errorf("sync: upsert %s: %w", entity, err)
	}
	return nil
}

// canonicalJSON reescreve um documento JSON numa forma canônica (chaves ordenadas, sem espaços),
// para comparar valores e não a formatação. Se não for JSON válido, devolve os bytes originais.
func canonicalJSON(b []byte) []byte {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber() // preserva inteiros grandes (sem passar por float64)
	var v any
	if err := dec.Decode(&v); err != nil {
		return b
	}
	out, err := json.Marshal(v)
	if err != nil {
		return b
	}
	return out
}
