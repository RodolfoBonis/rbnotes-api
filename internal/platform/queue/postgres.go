package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresQueue implementa Queue sobre a tabela jobs.
type PostgresQueue struct {
	pool    *pgxpool.Pool
	backoff BackoffConfig
}

// NewPostgres constrói a fila sobre o pool dado.
func NewPostgres(pool *pgxpool.Pool, backoff BackoffConfig) *PostgresQueue {
	if backoff.Base <= 0 {
		backoff = DefaultBackoff
	}
	return &PostgresQueue{pool: pool, backoff: backoff}
}

const (
	defaultMaxAttempts = 5
	maxErrorLen        = 2000
)

// returningCols são as colunas devolvidas pelo Enqueue/Claim (lease_token como texto para scan).
const returningCols = `id, kind, payload, status, run_at, attempts, max_attempts, last_error, COALESCE(lease_token::text, '')`

// Enqueue insere um job, respeitando o dedupe via ON CONFLICT no índice parcial único.
func (q *PostgresQueue) Enqueue(ctx context.Context, p EnqueueParams) (*Job, bool, error) {
	payload := []byte("{}")
	if p.Payload != nil {
		b, err := json.Marshal(p.Payload)
		if err != nil {
			return nil, false, fmt.Errorf("marshal payload: %w", err)
		}
		payload = b
	}
	maxAttempts := p.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = defaultMaxAttempts
	}
	var runAt *time.Time
	if !p.RunAt.IsZero() {
		runAt = &p.RunAt
	}
	var dedupe *string
	if p.DedupeKey != "" {
		dedupe = &p.DedupeKey
	}

	// COALESCE($3, now()): quando RunAt é zero, o default é o now() do BANCO (relógio único).
	stmt := `
INSERT INTO jobs (kind, payload, status, run_at, max_attempts, dedupe_key)
VALUES ($1, $2, 'queued', COALESCE($3, now()), $4, $5)
ON CONFLICT (dedupe_key) WHERE dedupe_key IS NOT NULL AND status IN ('queued','running')
DO NOTHING
RETURNING ` + returningCols

	row := q.pool.QueryRow(ctx, stmt, p.Kind, payload, runAt, maxAttempts, dedupe)
	job, err := scanJob(row)
	if errors.Is(err, pgx.ErrNoRows) {
		// Conflito de dedupe: já há um job ativo idêntico.
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("enqueue job: %w", err)
	}
	return job, true, nil
}

// Claim reivindica o próximo job elegível. Antes, move para 'dead' os jobs com lease expirado e
// tentativas esgotadas (poison guard). Depois pega um job queued pronto OU running com lease
// expirado e tentativas restantes, incrementando attempts e gerando um novo lease_token.
func (q *PostgresQueue) Claim(ctx context.Context, lease time.Duration, kinds ...string) (*Job, error) {
	// Poison guard: um job cujo worker morreu (lease expirado) e que já esgotou as tentativas não
	// deve ser reivindicado de novo — vai direto para 'dead'.
	const poisonStmt = `
UPDATE jobs SET
    status = 'dead',
    locked_until = NULL,
    updated_at = now(),
    last_error = CASE WHEN last_error = '' THEN 'lease expirado sem tentativas restantes' ELSE last_error END
WHERE status = 'running' AND locked_until < now() AND attempts >= max_attempts`
	if _, err := q.pool.Exec(ctx, poisonStmt); err != nil {
		return nil, fmt.Errorf("poison guard: %w", err)
	}

	stmt := `
UPDATE jobs SET
    status = 'running',
    locked_until = now() + $1::interval,
    lease_token = gen_random_uuid(),
    attempts = attempts + 1,
    updated_at = now()
WHERE id = (
    SELECT id FROM jobs
    WHERE run_at <= now()
      AND (
        status = 'queued'
        OR (status = 'running' AND locked_until < now() AND attempts < max_attempts)
      )
      AND (cardinality($2::text[]) = 0 OR kind = ANY($2::text[]))
    ORDER BY run_at
    FOR UPDATE SKIP LOCKED
    LIMIT 1
)
RETURNING ` + returningCols

	if kinds == nil {
		kinds = []string{}
	}
	row := q.pool.QueryRow(ctx, stmt, lease, kinds)
	job, err := scanJob(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim job: %w", err)
	}
	return job, nil
}

// Complete marca o job como concluído. Exige o lease_token (posse) — 0 linhas → ErrLeaseLost.
func (q *PostgresQueue) Complete(ctx context.Context, id, leaseToken string) error {
	tag, err := q.pool.Exec(ctx,
		`UPDATE jobs SET status='done', locked_until=NULL, last_error='', updated_at=now()
         WHERE id=$1 AND status='running' AND lease_token=$2`, id, leaseToken)
	if err != nil {
		return fmt.Errorf("complete job: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	return nil
}

// Fail reagenda o job com backoff enquanto houver tentativas; esgotadas (ou erro Permanent), marca
// 'dead'. O backoff é calculado no próprio statement (sem leitura extra de attempts). Exige o
// lease_token — 0 linhas → ErrLeaseLost.
func (q *PostgresQueue) Fail(ctx context.Context, id, leaseToken string, cause error) error {
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}

	if IsPermanent(cause) {
		tag, err := q.pool.Exec(ctx,
			`UPDATE jobs SET status='dead', locked_until=NULL, last_error=$3, updated_at=now()
             WHERE id=$1 AND status='running' AND lease_token=$2`,
			id, leaseToken, truncateRunes(msg, maxErrorLen))
		if err != nil {
			return fmt.Errorf("fail job (permanente): %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrLeaseLost
		}
		return nil
	}

	baseSecs := q.backoff.Base.Seconds()
	maxSecs := q.backoff.Max.Seconds()
	const stmt = `
UPDATE jobs SET
    status = CASE WHEN attempts >= max_attempts THEN 'dead' ELSE 'queued' END,
    run_at = CASE
        WHEN attempts >= max_attempts THEN run_at
        ELSE now() + make_interval(secs =>
            LEAST($4::float8, $5::float8 * power(2, attempts - 1))
            * (1 + $6::float8 * (2 * random() - 1)))
    END,
    locked_until = NULL,
    last_error = $3,
    updated_at = now()
WHERE id = $1 AND status = 'running' AND lease_token = $2`
	tag, err := q.pool.Exec(ctx, stmt, id, leaseToken, truncateRunes(msg, maxErrorLen), maxSecs, baseSecs, q.backoff.Jitter)
	if err != nil {
		return fmt.Errorf("fail job: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	return nil
}

// Cleanup apaga jobs terminais antigos (housekeeping periódico): done depois de doneAge, dead depois
// de deadAge (dead fica mais tempo — é o que se investiga). Em lotes, para não travar a tabela.
func (q *PostgresQueue) Cleanup(ctx context.Context, doneAge, deadAge time.Duration) (int64, error) {
	var total int64
	for {
		tag, err := q.pool.Exec(ctx,
			`DELETE FROM jobs WHERE id IN (
                SELECT id FROM jobs
                WHERE (status = 'done' AND updated_at < now() - make_interval(secs => $1::float8))
                   OR (status = 'dead' AND updated_at < now() - make_interval(secs => $2::float8))
                LIMIT 500)`,
			doneAge.Seconds(), deadAge.Seconds())
		if err != nil {
			return total, fmt.Errorf("cleanup jobs: %w", err)
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < 500 {
			return total, nil
		}
	}
}

// ErrNotRetryable indica que o job não existe ou não está dead.
var ErrNotRetryable = errors.New("queue: job não está dead")

// Retry devolve um job dead para a fila (tentativas zeradas). Usado pelo painel do dono.
func (q *PostgresQueue) Retry(ctx context.Context, id string) error {
	tag, err := q.pool.Exec(ctx,
		`UPDATE jobs SET status = 'queued', attempts = 0, run_at = now(), locked_until = NULL,
                last_error = '(reprocessado) ' || coalesce(last_error, ''), updated_at = now()
         WHERE id = $1 AND status = 'dead'`, id)
	if err != nil {
		return fmt.Errorf("retry job: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotRetryable
	}
	return nil
}

// Requeue reagenda o job para runAt devolvendo a tentativa gasta no claim (GREATEST(attempts-1,0)):
// um limite externo (cota) não deve empurrar o job para 'dead'. Exige o lease_token.
func (q *PostgresQueue) Requeue(ctx context.Context, id, leaseToken string, runAt time.Time, cause error) error {
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}
	tag, err := q.pool.Exec(ctx,
		`UPDATE jobs SET status='queued', run_at=$3, attempts=GREATEST(attempts-1, 0),
                locked_until=NULL, last_error=$4, updated_at=now()
         WHERE id=$1 AND status='running' AND lease_token=$2`,
		id, leaseToken, runAt, truncateRunes(msg, maxErrorLen))
	if err != nil {
		return fmt.Errorf("requeue job: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	return nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanJob(row rowScanner) (*Job, error) {
	var j Job
	var payload []byte
	var runAt time.Time
	if err := row.Scan(&j.ID, &j.Kind, &payload, &j.Status, &runAt, &j.Attempts, &j.MaxAttempts, &j.LastError, &j.LeaseToken); err != nil {
		return nil, err
	}
	j.Payload = payload
	j.RunAt = runAt
	return &j, nil
}

// truncateRunes corta s em no máximo n bytes SEM partir um code point UTF-8 no meio.
func truncateRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Recua até o início de um code point válido.
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
