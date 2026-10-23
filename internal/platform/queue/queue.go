// Package queue é uma fila de jobs durável em Postgres.
//
// Garantias:
//   - Claim atômico com FOR UPDATE SKIP LOCKED: réplicas concorrentes nunca pegam o mesmo job.
//   - Lease (locked_until) + lease_token: se um worker morre segurando um job, o lease expira e
//     outro reivindica. Complete/Fail exigem o lease_token do claim — um worker "zumbi" (lease já
//     expirado e re-reivindicado por outro) não consegue marcar done/fail e recebe ErrLeaseLost.
//   - Poison guard: no claim, jobs com lease expirado E attempts esgotadas vão para 'dead' em vez
//     de serem reivindicados de novo (evita loop de job envenenado).
//   - Dedupe: Enqueue com DedupeKey não-vazio é no-op se já existe um job idêntico queued/running
//     (índice parcial único uq_jobs_dedupe_active).
//   - Backoff exponencial com jitter até max_attempts; exaurido → estado 'dead'. Erros Permanent
//     vão direto para 'dead' sem consumir tentativas.
//
// A interface Queue isola a implementação para permitir trocar por outro broker depois.
package queue

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// ErrLeaseLost indica que Complete/Fail não afetou o job: ele não está mais 'running' sob o
// lease_token informado (outro worker o reivindicou após o lease expirar). O chamador deve parar
// de processá-lo, sem erro fatal.
var ErrLeaseLost = errors.New("queue: lease perdido (job reivindicado por outro worker)")

// Status é o estado de um job.
type Status string

// Estados possíveis de um job.
const (
	StatusQueued  Status = "queued"
	StatusRunning Status = "running"
	StatusDone    Status = "done"
	StatusDead    Status = "dead"
)

// permanentError marca um erro que NÃO deve ser retentado: o job vai direto para 'dead'.
type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

// Permanent embrulha um erro para sinalizar que o job deve morrer imediatamente (sem retry).
// Use para falhas que não se resolvem com nova tentativa (credencial inválida, perfil privado).
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &permanentError{err: err}
}

// IsPermanent indica se err (ou algum da cadeia) foi marcado como Permanent.
func IsPermanent(err error) bool {
	var pe *permanentError
	return errors.As(err, &pe)
}

// Job é uma unidade de trabalho persistida.
type Job struct {
	ID          string
	Kind        string
	Payload     json.RawMessage
	Status      Status
	RunAt       time.Time
	Attempts    int
	MaxAttempts int
	LastError   string
	// LeaseToken identifica a posse atual do job (devolvido pelo Claim; exigido por Complete/Fail).
	LeaseToken string
}

// EnqueueParams descreve um job a enfileirar.
type EnqueueParams struct {
	Kind string
	// Payload é serializado em JSON. Pode ser nil (vira {}).
	Payload any
	// DedupeKey, quando não-vazio, impede enfileirar um job idêntico já queued/running.
	DedupeKey string
	// MaxAttempts default 5 quando <= 0.
	MaxAttempts int
	// RunAt agenda o job para o futuro (zero = agora, resolvido pelo now() do banco).
	RunAt time.Time
}

// Queue é a fila de jobs. As implementações devem ser seguras para uso concorrente.
type Queue interface {
	// Enqueue insere um job. Com DedupeKey e um job ativo idêntico existente, devolve
	// (nil, false, nil) — enfileiramento suprimido, sem erro.
	Enqueue(ctx context.Context, p EnqueueParams) (job *Job, enqueued bool, err error)
	// Claim reivindica o próximo job elegível (queued ou com lease expirado e tentativas restantes),
	// marcando-o running com um novo lease + lease_token. Devolve (nil, nil) quando não há job.
	// kinds restringe aos tipos dados (vazio = qualquer): o worker de sync e o ai-worker dividem a
	// mesma tabela sem pegar o job um do outro.
	Claim(ctx context.Context, lease time.Duration, kinds ...string) (*Job, error)
	// Complete marca o job como concluído. Devolve ErrLeaseLost se o lease_token não bater.
	Complete(ctx context.Context, id, leaseToken string) error
	// Fail registra uma falha: reagenda com backoff enquanto houver tentativas; esgotadas (ou erro
	// Permanent), marca 'dead'. Devolve ErrLeaseLost se o lease_token não bater.
	Fail(ctx context.Context, id, leaseToken string, cause error) error
	// Cleanup remove jobs done mais antigos que doneAge e dead mais antigos que deadAge. Devolve
	// quantos apagou.
	Cleanup(ctx context.Context, doneAge, deadAge time.Duration) (int64, error)
	// Retry devolve um job dead para a fila; ErrNotRetryable se não existir ou não estiver dead.
	Retry(ctx context.Context, id string) error
}

// BackoffConfig parametriza o backoff exponencial com jitter.
type BackoffConfig struct {
	Base   time.Duration // atraso base (ex.: 5s)
	Max    time.Duration // teto do atraso (ex.: 1h)
	Jitter float64       // fração de jitter [0,1] aplicada ao atraso (ex.: 0.2 = ±20%)
}

// DefaultBackoff é o backoff padrão da fila.
var DefaultBackoff = BackoffConfig{Base: 5 * time.Second, Max: time.Hour, Jitter: 0.2}
