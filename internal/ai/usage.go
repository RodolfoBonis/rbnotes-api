package ai

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	dbgen "github.com/RodolfoBonis/rbnotes-api/db/gen"
)

// UsageQuery são os parâmetros resolvidos de GET /v1/ai/usage. As datas vêm validadas pela borda;
// FromTS/ToTS são os instantes (início de dia no fuso) do intervalo semiaberto [FromTS, ToTS).
type UsageQuery struct {
	UserID   pgtype.UUID
	TZ       string    // nome IANA (usado no AT TIME ZONE e devolvido no eco)
	FromDate string    // YYYY-MM-DD inclusivo (eco)
	ToDate   string    // YYYY-MM-DD inclusivo (eco)
	FromTS   time.Time // início de FromDate no fuso
	ToTS     time.Time // início do dia SEGUINTE a ToDate no fuso (limite superior exclusivo)
}

// UsageQuota é a cota diária (24 h) do usuário.
type UsageQuota struct {
	Limit   int   `json:"limit"`
	Used24h int64 `json:"used24h"`
}

// UsageTotals são os totais do período.
type UsageTotals struct {
	Requests            int64   `json:"requests"`
	Done                int64   `json:"done"`
	Errors              int64   `json:"errors"`
	InputTokens         int64   `json:"inputTokens"`
	OutputTokens        int64   `json:"outputTokens"`
	CacheCreationTokens int64   `json:"cacheCreationTokens"`
	CacheReadTokens     int64   `json:"cacheReadTokens"`
	CostUsd             float64 `json:"costUsd"`
	DurationMs          int64   `json:"durationMs"`
}

// UsageKind é o consumo agregado de um tipo de pedido no período.
type UsageKind struct {
	Kind     string  `json:"kind"`
	Requests int64   `json:"requests"`
	CostUsd  float64 `json:"costUsd"`
}

// UsageDayKind é o consumo de um tipo de pedido num dia.
type UsageDayKind struct {
	Kind         string  `json:"kind"`
	Requests     int64   `json:"requests"`
	CostUsd      float64 `json:"costUsd"`
	InputTokens  int64   `json:"inputTokens"`
	OutputTokens int64   `json:"outputTokens"`
}

// UsageDay é a série diária (data no fuso pedido).
type UsageDay struct {
	Date     string         `json:"date"`
	Requests int64          `json:"requests"`
	CostUsd  float64        `json:"costUsd"`
	Kinds    []UsageDayKind `json:"kinds"`
}

// UsageReport é a resposta de GET /v1/ai/usage. O custo é SEMPRE estimativa (preço de API; o dono
// usa assinatura), por isso costIsEstimate é fixo em true.
type UsageReport struct {
	From           string      `json:"from"`
	To             string      `json:"to"`
	TZ             string      `json:"tz"`
	Currency       string      `json:"currency"`
	CostIsEstimate bool        `json:"costIsEstimate"`
	Quota          UsageQuota  `json:"quota"`
	Totals         UsageTotals `json:"totals"`
	ByKind         []UsageKind `json:"byKind"`
	Days           []UsageDay  `json:"days"`
}

// Usage agrega o consumo de IA do usuário no período (escopo por usuário) e anexa a cota de 24 h. A
// série diária é calculada no fuso informado (AT TIME ZONE). As notas cifradas não distorcem nada:
// o consumo é por pedido de IA, independente de a entidade ser cifrada ou não.
func (s *Service) Usage(ctx context.Context, qy UsageQuery) (UsageReport, error) {
	rows, err := s.q.AIUsageByDay(ctx, dbgen.AIUsageByDayParams{
		Tz:     qy.TZ,
		UserID: qy.UserID,
		FromTs: pgtype.Timestamptz{Time: qy.FromTS, Valid: true},
		ToTs:   pgtype.Timestamptz{Time: qy.ToTS, Valid: true},
	})
	if err != nil {
		return UsageReport{}, fmt.Errorf("ai: usage: %w", err)
	}

	rep := UsageReport{
		From: qy.FromDate, To: qy.ToDate, TZ: qy.TZ, Currency: "USD", CostIsEstimate: true,
		ByKind: []UsageKind{}, Days: []UsageDay{},
	}

	byKind := map[string]*UsageKind{}
	var kindOrder []string
	var curDay *UsageDay

	for _, r := range rows {
		cost := numericToFloat(r.CostUsd)

		// Totais.
		rep.Totals.Requests += r.Requests
		rep.Totals.Done += r.Done
		rep.Totals.Errors += r.Errors
		rep.Totals.InputTokens += r.InputTokens
		rep.Totals.OutputTokens += r.OutputTokens
		rep.Totals.CacheCreationTokens += r.CacheCreationTokens
		rep.Totals.CacheReadTokens += r.CacheReadTokens
		rep.Totals.CostUsd += cost
		rep.Totals.DurationMs += r.DurationMs

		// byKind.
		k, ok := byKind[r.Kind]
		if !ok {
			k = &UsageKind{Kind: r.Kind}
			byKind[r.Kind] = k
			kindOrder = append(kindOrder, r.Kind)
		}
		k.Requests += r.Requests
		k.CostUsd += cost

		// Série diária (as linhas vêm ordenadas por day, kind).
		date := formatDate(r.Day)
		if curDay == nil || curDay.Date != date {
			rep.Days = append(rep.Days, UsageDay{Date: date, Kinds: []UsageDayKind{}})
			curDay = &rep.Days[len(rep.Days)-1]
		}
		curDay.Requests += r.Requests
		curDay.CostUsd += cost
		curDay.Kinds = append(curDay.Kinds, UsageDayKind{
			Kind: r.Kind, Requests: r.Requests, CostUsd: round6(cost),
			InputTokens: r.InputTokens, OutputTokens: r.OutputTokens,
		})
	}

	// byKind ordenado por custo desc, depois kind (determinístico).
	for _, kind := range kindOrder {
		k := byKind[kind]
		rep.ByKind = append(rep.ByKind, UsageKind{Kind: k.Kind, Requests: k.Requests, CostUsd: round6(k.CostUsd)})
	}
	sort.Slice(rep.ByKind, func(i, j int) bool {
		if rep.ByKind[i].CostUsd != rep.ByKind[j].CostUsd {
			return rep.ByKind[i].CostUsd > rep.ByKind[j].CostUsd
		}
		return rep.ByKind[i].Kind < rep.ByKind[j].Kind
	})

	rep.Totals.CostUsd = round6(rep.Totals.CostUsd)
	for i := range rep.Days {
		rep.Days[i].CostUsd = round6(rep.Days[i].CostUsd)
	}

	used, err := s.q.CountAIRequestsSince(ctx, dbgen.CountAIRequestsSinceParams{
		UserID: qy.UserID, CreatedAt: pgtype.Timestamptz{Time: time.Now().Add(-24 * time.Hour), Valid: true},
	})
	if err != nil {
		return UsageReport{}, fmt.Errorf("ai: usage quota: %w", err)
	}
	rep.Quota = UsageQuota{Limit: s.dailyLimit, Used24h: used}
	return rep, nil
}

// formatDate formata a data (sem fuso) devolvida pelo AT TIME ZONE como YYYY-MM-DD.
func formatDate(d pgtype.Date) string {
	if !d.Valid {
		return ""
	}
	return d.Time.Format("2006-01-02")
}

// round6 arredonda para 6 casas (precisão da coluna numeric(12,6)), evitando ruído de float na soma.
func round6(f float64) float64 {
	return math.Round(f*1e6) / 1e6
}
