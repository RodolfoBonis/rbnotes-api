package ai

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/require"

	dbgen "github.com/RodolfoBonis/rbnotes-api/db/gen"
	"github.com/RodolfoBonis/rbnotes-api/internal/testsupport"
)

// TestSetAIJobUsageAccumulates prova que SetAIJobUsage SOMA o consumo (duas execuções, como no retry
// sem --resume do chat) e que o resolved_model só é sobrescrito por um valor não-vazio.
func TestSetAIJobUsageAccumulates(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	q := dbgen.New(pool)
	ctx := context.Background()
	u, err := q.CreateUser(ctx, dbgen.CreateUserParams{Issuer: "i", Subject: "s-" + t.Name(), Email: "a@x", Name: "A"})
	require.NoError(t, err)

	job, err := q.CreateAIJob(ctx, dbgen.CreateAIJobParams{UserID: u.ID, Kind: KindChat, Input: []byte("{}")})
	require.NoError(t, err)

	require.NoError(t, q.SetAIJobUsage(ctx, dbgen.SetAIJobUsageParams{
		ID: job.ID, CostUsd: numericFromFloat(0.01), InputTokens: 10, OutputTokens: 20,
		CacheCreationTokens: 5, CacheReadTokens: 1, DurationMs: 100, NumTurns: 1, ResolvedModel: "claude-haiku-4-5",
	}))
	// Segunda execução (retry): acumula; resolved_model vazio NÃO apaga o anterior.
	require.NoError(t, q.SetAIJobUsage(ctx, dbgen.SetAIJobUsageParams{
		ID: job.ID, CostUsd: numericFromFloat(0.02), InputTokens: 30, OutputTokens: 40,
		CacheCreationTokens: 2, CacheReadTokens: 3, DurationMs: 50, NumTurns: 1, ResolvedModel: "",
	}))

	got, err := q.GetAIJob(ctx, job.ID)
	require.NoError(t, err)
	require.Equal(t, int64(40), got.InputTokens)
	require.Equal(t, int64(60), got.OutputTokens)
	require.Equal(t, int64(7), got.CacheCreationTokens)
	require.Equal(t, int64(4), got.CacheReadTokens)
	require.Equal(t, int32(150), got.DurationMs)
	require.Equal(t, int32(2), got.NumTurns)
	require.Equal(t, "claude-haiku-4-5", got.ResolvedModel)
	require.InDelta(t, 0.03, numericToFloat(got.CostUsd), 1e-9)
}

// TestUsageAggregates cobre a agregação de GET /v1/ai/usage: totais, série diária por kind (no fuso
// pedido), byKind e cota. Escopo por usuário (jobs de outro usuário não entram).
func TestUsageAggregates(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	q := dbgen.New(pool)
	ctx := context.Background()
	u, err := q.CreateUser(ctx, dbgen.CreateUserParams{Issuer: "i", Subject: "s-" + t.Name(), Email: "a@x", Name: "A"})
	require.NoError(t, err)
	other, err := q.CreateUser(ctx, dbgen.CreateUserParams{Issuer: "i", Subject: "s-other-" + t.Name(), Email: "b@x", Name: "B"})
	require.NoError(t, err)

	svc := New(Deps{Queries: q, Pool: pool, DailyLimit: 200, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	loc, err := time.LoadLocation("America/Sao_Paulo")
	require.NoError(t, err)

	mk := func(userID pgtype.UUID, kind, status string, created time.Time, cost float64, in, out int64) {
		j, cerr := q.CreateAIJob(ctx, dbgen.CreateAIJobParams{UserID: userID, Kind: kind, Input: []byte("{}")})
		require.NoError(t, cerr)
		require.NoError(t, q.SetAIJobUsage(ctx, dbgen.SetAIJobUsageParams{
			ID: j.ID, CostUsd: numericFromFloat(cost), InputTokens: in, OutputTokens: out,
			DurationMs: 10, NumTurns: 1, ResolvedModel: "m",
		}))
		_, uerr := pool.Exec(ctx, "UPDATE ai_jobs SET status = $2, created_at = $3 WHERE id = $1", j.ID, status, created)
		require.NoError(t, uerr)
	}

	// Dia 2026-10-20 (local SP): enrich done + chat error. Dia 2026-10-21: ask done.
	mk(u.ID, KindEnrich, "done", time.Date(2026, 10, 20, 15, 0, 0, 0, loc), 0.10, 100, 50)
	mk(u.ID, KindChat, "error", time.Date(2026, 10, 20, 23, 30, 0, 0, loc), 0.05, 10, 0)
	mk(u.ID, KindAsk, "done", time.Date(2026, 10, 21, 9, 0, 0, 0, loc), 0.20, 200, 80)
	// Outro usuário: não deve aparecer.
	mk(other.ID, KindEnrich, "done", time.Date(2026, 10, 20, 12, 0, 0, 0, loc), 9.99, 999, 999)

	fromMid := time.Date(2026, 10, 19, 0, 0, 0, 0, loc)
	toMid := time.Date(2026, 10, 22, 0, 0, 0, 0, loc)
	rep, err := svc.Usage(ctx, UsageQuery{
		UserID: u.ID, TZ: "America/Sao_Paulo", FromDate: "2026-10-19", ToDate: "2026-10-22",
		FromTS: fromMid, ToTS: toMid.AddDate(0, 0, 1),
	})
	require.NoError(t, err)

	require.Equal(t, "USD", rep.Currency)
	require.True(t, rep.CostIsEstimate)
	require.Equal(t, "America/Sao_Paulo", rep.TZ)
	require.Equal(t, 200, rep.Quota.Limit)

	require.Equal(t, int64(3), rep.Totals.Requests)
	require.Equal(t, int64(2), rep.Totals.Done)
	require.Equal(t, int64(1), rep.Totals.Errors)
	require.Equal(t, int64(310), rep.Totals.InputTokens)
	require.Equal(t, int64(130), rep.Totals.OutputTokens)
	require.InDelta(t, 0.35, rep.Totals.CostUsd, 1e-9)

	require.Len(t, rep.Days, 2)
	require.Equal(t, "2026-10-20", rep.Days[0].Date)
	require.Equal(t, int64(2), rep.Days[0].Requests)
	require.InDelta(t, 0.15, rep.Days[0].CostUsd, 1e-9)
	require.Equal(t, "2026-10-21", rep.Days[1].Date)
	require.Equal(t, int64(1), rep.Days[1].Requests)

	// byKind ordenado por custo desc: ask (0.20) > enrich (0.10) > chat (0.05).
	require.Len(t, rep.ByKind, 3)
	require.Equal(t, KindAsk, rep.ByKind[0].Kind)
	require.InDelta(t, 0.20, rep.ByKind[0].CostUsd, 1e-9)
}
