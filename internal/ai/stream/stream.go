// Package stream transporta a resposta da IA do ai-worker para a API (e dela para o browser via SSE)
// por Redis Streams: uma stream por resposta (`ai:stream:<id>`, id = guia ou mensagem do chat).
//
// O worker publica pedaços (text/json), avisos (tool/status) e um evento final (done/error). A API lê
// do início — ou do último id recebido (Last-Event-ID), para o browser reconectar sem perder nada.
// A stream expira 1h após a última escrita: o resultado definitivo fica no Postgres.
package stream

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Tipos de evento.
const (
	TypeText   = "text"   // pedaço de texto (chat)
	TypeJSON   = "json"   // pedaço do JSON estruturado (guias)
	TypeTool   = "tool"   // o modelo usou uma ferramenta ("Pesquisando: …")
	TypeStatus = "status" // aviso do worker ("Na fila", "Limite de uso — tentando de novo")
	TypeDone   = "done"   // fim com sucesso (o resultado está no banco)
	TypeError  = "error"  // fim com erro (mensagem)
)

const (
	ttl    = time.Hour
	maxLen = 20000
	// flushEvery agrega deltas pequenos (a CLI manda poucos caracteres por evento).
	flushEvery = 120 * time.Millisecond
)

// Key é a chave Redis da stream de uma resposta.
func Key(id string) string { return "ai:stream:" + id }

// Event é um item da stream.
type Event struct {
	ID   string `json:"-"`
	Type string `json:"type"`
	Data string `json:"data"`
}

// Final indica se o evento encerra a resposta.
func (e Event) Final() bool { return e.Type == TypeDone || e.Type == TypeError }

// Publisher escreve na stream de uma resposta, agregando deltas (text/json) a cada flushEvery.
// Seguro para uso concorrente; Close encerra com o evento final.
type Publisher struct {
	rdb *redis.Client
	key string

	mu      sync.Mutex
	pending strings.Builder
	pType   string
	timer   *time.Timer
	err     error
}

// NewPublisher abre a stream da resposta id. Uma nova geração (regenerar) começa do zero.
func NewPublisher(ctx context.Context, rdb *redis.Client, id string) *Publisher {
	p := &Publisher{rdb: rdb, key: Key(id)}
	p.err = rdb.Del(ctx, p.key).Err()
	return p
}

// Emit publica um evento. Deltas do mesmo tipo são agregados; outros tipos descarregam antes.
func (p *Publisher) Emit(ctx context.Context, typ, data string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if typ == TypeText || typ == TypeJSON {
		if p.pType != "" && p.pType != typ {
			p.flushLocked(ctx)
		}
		p.pType = typ
		p.pending.WriteString(data)
		if p.timer == nil {
			p.timer = time.AfterFunc(flushEvery, func() {
				p.mu.Lock()
				defer p.mu.Unlock()
				p.flushLocked(context.WithoutCancel(ctx))
			})
		}
		return
	}
	p.flushLocked(ctx)
	p.addLocked(ctx, typ, data)
}

// Close descarrega o que falta e publica o evento final (done ou error).
func (p *Publisher) Close(ctx context.Context, typ, data string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.flushLocked(ctx)
	p.addLocked(ctx, typ, data)
	return p.err
}

func (p *Publisher) flushLocked(ctx context.Context) {
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
	if p.pending.Len() == 0 {
		return
	}
	p.addLocked(ctx, p.pType, p.pending.String())
	p.pending.Reset()
	p.pType = ""
}

func (p *Publisher) addLocked(ctx context.Context, typ, data string) {
	pipe := p.rdb.TxPipeline()
	pipe.XAdd(ctx, &redis.XAddArgs{Stream: p.key, MaxLen: maxLen, Approx: true, Values: map[string]any{"t": typ, "d": data}})
	pipe.Expire(ctx, p.key, ttl)
	if _, err := pipe.Exec(ctx); err != nil && p.err == nil {
		p.err = err
	}
}

// Read devolve os eventos após lastID ("0" = do início), esperando até block por novos. Sem
// eventos no período, devolve vazio (o chamador manda heartbeat e tenta de novo).
func Read(ctx context.Context, rdb *redis.Client, id, lastID string, block time.Duration) ([]Event, error) {
	if lastID == "" {
		lastID = "0"
	}
	res, err := rdb.XRead(ctx, &redis.XReadArgs{Streams: []string{Key(id), lastID}, Count: 500, Block: block}).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Event
	for _, s := range res {
		for _, m := range s.Messages {
			t, _ := m.Values["t"].(string)
			d, _ := m.Values["d"].(string)
			out = append(out, Event{ID: m.ID, Type: t, Data: d})
		}
	}
	return out, nil
}
