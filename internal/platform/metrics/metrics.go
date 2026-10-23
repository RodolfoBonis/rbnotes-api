// Package metrics define os coletores Prometheus do rbnotes e o handler /metrics.
//
// Os coletores vivem num registry dedicado (não o default global), exposto por Handler(). As
// funções de registro são livres de alocação no caminho quente e seguras para uso concorrente.
// O /metrics é servido SÓ no listener interno (METRICS_ADDR): nunca no host público.
package metrics

import (
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	registry = prometheus.NewRegistry()

	// jobsProcessed conta jobs processados pela fila, por kind e resultado (ok|retry|dead).
	jobsProcessed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "jobs_processed_total",
		Help: "Jobs processados pela fila, por kind e resultado.",
	}, []string{"kind", "result"})

	// aiJobs conta execuções da IA por tipo (note_enrich|note_chat|ask|...) e resultado
	// (ok|error|rate_limited).
	aiJobs = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "ai_jobs_total",
		Help: "Execuções da IA, por tipo e resultado.",
	}, []string{"kind", "result"})

	// httpRequests / httpDuration: tráfego da API por rota (padrão do chi, não o path cru — sem
	// explosão de cardinalidade) e classe de status. A stream SSE da IA fica fora da latência.
	httpRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "Requisições HTTP da API, por método, rota e classe de status (2xx..5xx).",
	}, []string{"method", "route", "code"})
	httpDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_request_duration_seconds",
		Help:    "Latência das requisições HTTP da API, por método e rota.",
		Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
	}, []string{"method", "route"})

	// jobsQueue (só no ai-worker, lido do banco a cada minuto): fila de jobs por tipo e status.
	jobsQueue = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "jobs_queue",
		Help: "Jobs na fila por tipo e status (queued|running|dead).",
	}, []string{"kind", "status"})

	// dependencyUp sinaliza a disponibilidade das dependências externas opcionais (1 = ok, 0 = fora).
	dependencyUp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "rbnotes_dependency_up",
		Help: "1 se a dependência externa está disponível, 0 caso contrário.",
	}, []string{"dep"})
	stateOnce sync.Once
)

// Kinds de job de IA e resultados, usados para pré-inicializar os labels das métricas (aparecem com
// 0 antes do primeiro evento, para alertas de ausência funcionarem).
var (
	aiKinds    = []string{"note_enrich", "note_chat", "ask", "collection_rule", "diary_month", "daily_prompt"}
	aiResults  = []string{"ok", "error", "rate_limited"}
	jobResults = []string{"ok", "retry", "dead", "rate_limited", "interrupted"}
	deps       = []string{"redis", "minio"}
)

func init() {
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	registry.MustRegister(jobsProcessed, aiJobs, httpRequests, httpDuration, dependencyUp)

	// Pré-inicialização dos labels: cada série nasce em 0, para um alerta de "métrica ausente" (ver
	// RbNotesApiMetricsMissing) distinguir "sem tráfego" de "processo morto".
	for _, k := range aiKinds {
		for _, r := range aiResults {
			aiJobs.WithLabelValues(k, r)
		}
		for _, r := range jobResults {
			jobsProcessed.WithLabelValues("ai_job", r)
		}
	}
	for _, d := range deps {
		dependencyUp.WithLabelValues(d).Set(1)
	}
}

// SetDependencyUp marca uma dependência externa (redis|minio) como disponível ou não.
func SetDependencyUp(dep string, up bool) {
	v := 0.0
	if up {
		v = 1
	}
	dependencyUp.WithLabelValues(dep).Set(v)
}

// JobProcessed registra o resultado do processamento de um job.
func JobProcessed(kind, result string) {
	jobsProcessed.WithLabelValues(kind, result).Inc()
}

// AIJob registra uma execução da IA.
func AIJob(kind, result string) {
	aiJobs.WithLabelValues(kind, result).Inc()
}

// ObserveHTTP registra uma requisição da API. withLatency=false para conexões longas (SSE).
func ObserveHTTP(method, route string, status int, d time.Duration, withLatency bool) {
	httpRequests.WithLabelValues(method, route, statusClass(status)).Inc()
	if withLatency {
		httpDuration.WithLabelValues(method, route).Observe(d.Seconds())
	}
}

func statusClass(status int) string {
	switch {
	case status >= 500:
		return "5xx"
	case status >= 400:
		return "4xx"
	case status >= 300:
		return "3xx"
	default:
		return "2xx"
	}
}

// JobQueueCount é uma linha da contagem da fila.
type JobQueueCount struct {
	Kind, Status string
	N            int64
}

// SetState publica o retrato atual da fila (substitui o anterior). Só o ai-worker chama.
func SetState(queue []JobQueueCount) {
	stateOnce.Do(func() { registry.MustRegister(jobsQueue) })
	jobsQueue.Reset()
	for _, q := range queue {
		jobsQueue.WithLabelValues(q.Kind, q.Status).Set(float64(q.N))
	}
}

// Handler retorna o http.Handler que expõe /metrics no formato Prometheus.
func Handler() http.Handler {
	return promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
}
