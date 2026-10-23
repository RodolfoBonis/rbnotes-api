package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	stdsync "sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	dbgen "github.com/RodolfoBonis/rbnotes-api/db/gen"
	syncpkg "github.com/RodolfoBonis/rbnotes-api/internal/sync"
	"github.com/RodolfoBonis/rbnotes-api/internal/testsupport"
)

// TestWriteEntityChangeSeqNeverSkipsPull prova que a alocação de change_seq do enrich da IA, feita na
// MESMA transação do upsert, nunca deixa o pull do sync pular uma linha — mesmo sob Push concorrente.
//
// Vários produtores gravam entidades ao mesmo tempo (metade via writeEntity da IA, metade via Push
// do sync), todos no MESMO usuário (contenção no sync_cursors). Um consumidor puxa em paralelo,
// avançando o cursor como o desktop faria. Se um change_seq N pudesse ficar visível DEPOIS de o
// cursor já ter passado dele, a linha sumiria para sempre e o conjunto final de ids seria menor.
func TestWriteEntityChangeSeqNeverSkipsPull(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	q := dbgen.New(pool)
	ctx := context.Background()

	u, err := q.CreateUser(ctx, dbgen.CreateUserParams{Issuer: "i", Subject: "s-" + t.Name(), Email: "a@x", Name: "A"})
	require.NoError(t, err)
	uid := u.ID

	svc := New(Deps{Queries: q, Pool: pool, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	syncSvc := syncpkg.New(q, pool)

	const total = 120
	want := make(map[string]bool, total)
	for i := 0; i < total; i++ {
		want[fmt.Sprintf("id-%d", i)] = true
	}

	// Consumidor concorrente: puxa continuamente avançando o cursor, coletando ids vistos.
	seen := make(map[string]bool, total)
	var seenMu stdsync.Mutex
	stop := make(chan struct{})
	pullerDone := make(chan struct{})
	go func() {
		defer close(pullerDone)
		cursor := ""
		for {
			res, perr := syncSvc.Pull(ctx, uid, cursor)
			if perr == nil {
				cursor = res.Cursor
				seenMu.Lock()
				for _, c := range res.Changes {
					seen[c.ID] = true
				}
				n := len(seen)
				seenMu.Unlock()
				if n >= total {
					return
				}
			}
			select {
			case <-stop:
				// Dreno final: continua paginando até esvaziar.
				for {
					res, perr := syncSvc.Pull(ctx, uid, cursor)
					if perr != nil || len(res.Changes) == 0 {
						return
					}
					cursor = res.Cursor
					seenMu.Lock()
					for _, c := range res.Changes {
						seen[c.ID] = true
					}
					seenMu.Unlock()
				}
			case <-time.After(2 * time.Millisecond):
			}
		}
	}()

	// Produtores concorrentes.
	var wg stdsync.WaitGroup
	sem := make(chan struct{}, 16)
	for i := 0; i < total; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			id := fmt.Sprintf("id-%d", i)
			if i%2 == 0 {
				data, _ := json.Marshal(map[string]any{"title": id, "tldr": "x"})
				require.NoError(t, svc.writeEntity(ctx, uid, "note_ai", id, data))
				return
			}
			data, _ := json.Marshal(map[string]any{"title": id})
			_, perr := syncSvc.Push(ctx, uid, []syncpkg.Change{{
				Entity: "note", ID: id, UpdatedAt: time.Now().UTC(), Data: data,
			}})
			require.NoError(t, perr)
		}(i)
	}
	wg.Wait()
	close(stop)

	select {
	case <-pullerDone:
	case <-time.After(10 * time.Second):
		t.Fatal("puller não terminou: provável linha pulada (change_seq ficou visível fora de ordem)")
	}

	seenMu.Lock()
	defer seenMu.Unlock()
	require.Equal(t, total, len(seen), "o pull deve ver todas as entidades, sem pular nenhuma")
	for id := range want {
		require.True(t, seen[id], "id ausente no pull: %s", id)
	}
}
