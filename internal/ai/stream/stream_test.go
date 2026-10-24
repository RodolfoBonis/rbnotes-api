package stream_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/RodolfoBonis/rbnotes-api/internal/ai/stream"
	"github.com/RodolfoBonis/rbnotes-api/internal/testsupport"
)

func TestPublishAndRead(t *testing.T) {
	rdb := testsupport.NewRedis(t)
	ctx := context.Background()

	p := stream.NewPublisher(ctx, rdb, "g1")
	p.Emit(ctx, stream.TypeTool, "Pesquisando: hades")
	for _, chunk := range []string{`{"sum`, `mary":`, ` "ok"}`} {
		p.Emit(ctx, stream.TypeJSON, chunk)
	}
	require.NoError(t, p.Close(ctx, stream.TypeDone, ""))

	evs, err := stream.Read(ctx, rdb, "g1", "0", 100*time.Millisecond)
	require.NoError(t, err)
	require.Len(t, evs, 3, "deltas do mesmo tipo são agregados")
	require.Equal(t, stream.TypeTool, evs[0].Type)
	require.Equal(t, `{"summary": "ok"}`, evs[1].Data)
	require.True(t, evs[2].Final())

	// Retomada pelo último id recebido (Last-Event-ID): só o que veio depois.
	rest, err := stream.Read(ctx, rdb, "g1", evs[1].ID, 100*time.Millisecond)
	require.NoError(t, err)
	require.Len(t, rest, 1)

	ttl, err := rdb.TTL(ctx, stream.Key("g1")).Result()
	require.NoError(t, err)
	require.Greater(t, ttl, 50*time.Minute)

	// Regenerar começa do zero.
	p2 := stream.NewPublisher(ctx, rdb, "g1")
	require.NoError(t, p2.Close(ctx, stream.TypeError, "falhou"))
	evs, err = stream.Read(ctx, rdb, "g1", "0", 100*time.Millisecond)
	require.NoError(t, err)
	require.Len(t, evs, 1)
	require.Equal(t, "falhou", evs[0].Data)
}

func TestReadBlocksUntilTimeout(t *testing.T) {
	rdb := testsupport.NewRedis(t)
	start := time.Now()
	evs, err := stream.Read(context.Background(), rdb, "nada", "0", 300*time.Millisecond)
	require.NoError(t, err)
	require.Empty(t, evs)
	require.GreaterOrEqual(t, time.Since(start), 250*time.Millisecond)
}

func TestTimerFlush(t *testing.T) {
	rdb := testsupport.NewRedis(t)
	ctx := context.Background()
	p := stream.NewPublisher(ctx, rdb, "c1")
	p.Emit(ctx, stream.TypeText, "Olá")
	evs, err := stream.Read(ctx, rdb, "c1", "0", 2*time.Second)
	require.NoError(t, err)
	require.Len(t, evs, 1, "o delta sai sozinho após o intervalo de agregação")
	require.Equal(t, "Olá", evs[0].Data)
}

// TestEphemeralTTLIsShort: uma stream efêmera (nota CIFRADA) expira em ~60s, não em 1h — os deltas
// carregam texto claro e o resultado não fica no Postgres, então a stream some logo após o done.
func TestEphemeralTTLIsShort(t *testing.T) {
	rdb := testsupport.NewRedis(t)
	ctx := context.Background()

	p := stream.NewPublisher(ctx, rdb, "eph1", stream.Ephemeral())
	p.Emit(ctx, stream.TypeJSON, `{"title":"segredo"}`)
	require.NoError(t, p.Close(ctx, stream.TypeDone, `{"title":"segredo"}`))

	ttl, err := rdb.TTL(ctx, stream.Key("eph1")).Result()
	require.NoError(t, err)
	require.Greater(t, ttl, time.Duration(0), "a stream ainda existe para o cliente reconectar")
	require.LessOrEqual(t, ttl, 60*time.Second, "mas expira em ~60s, não em 1h")

	// Os deltas compartilham a mesma chave: expiram junto com o evento final.
	evs, err := stream.Read(ctx, rdb, "eph1", "0", 100*time.Millisecond)
	require.NoError(t, err)
	require.Len(t, evs, 2)
	require.True(t, evs[1].Final())
}
