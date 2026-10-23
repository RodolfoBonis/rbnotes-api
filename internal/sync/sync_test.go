package sync_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/require"

	dbgen "github.com/RodolfoBonis/rbnotes-api/db/gen"
	syncpkg "github.com/RodolfoBonis/rbnotes-api/internal/sync"
	"github.com/RodolfoBonis/rbnotes-api/internal/testsupport"
)

func TestPushPullAndConflict(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	q := dbgen.New(pool)
	svc := syncpkg.New(q, pool)
	ctx := context.Background()

	userA := createUser(t, q, "sub-a")
	userB := createUser(t, q, "sub-b")

	t1 := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	t0 := t1.Add(-time.Hour)
	t2 := t1.Add(time.Hour)

	// Primeiro push: aplica.
	res, err := svc.Push(ctx, userA, []syncpkg.Change{
		{Entity: syncpkg.EntityNote, ID: "n1", UpdatedAt: t1, Data: json.RawMessage(`{"title":"Olá"}`)},
	})
	require.NoError(t, err)
	require.Equal(t, []string{"n1"}, res.Applied)
	require.Empty(t, res.Conflicts)

	// Pull desde o início: vê a nota, cursor avança.
	pull, err := svc.Pull(ctx, userA, "")
	require.NoError(t, err)
	require.Len(t, pull.Changes, 1)
	require.Equal(t, "note", pull.Changes[0].Entity)
	require.NotEqual(t, "0", pull.Cursor)

	// Push mais ANTIGO: conflito, devolve a versão do servidor (t1).
	res, err = svc.Push(ctx, userA, []syncpkg.Change{
		{Entity: syncpkg.EntityNote, ID: "n1", UpdatedAt: t0, Data: json.RawMessage(`{"title":"velho"}`)},
	})
	require.NoError(t, err)
	require.Empty(t, res.Applied)
	require.Len(t, res.Conflicts, 1)
	require.WithinDuration(t, t1, res.Conflicts[0].UpdatedAt, time.Second)

	// Push mais NOVO: aplica.
	res, err = svc.Push(ctx, userA, []syncpkg.Change{
		{Entity: syncpkg.EntityNote, ID: "n1", UpdatedAt: t2, Data: json.RawMessage(`{"title":"novo"}`)},
	})
	require.NoError(t, err)
	require.Equal(t, []string{"n1"}, res.Applied)

	// Push com timestamp IGUAL: idempotente (aplicado sem conflito).
	res, err = svc.Push(ctx, userA, []syncpkg.Change{
		{Entity: syncpkg.EntityNote, ID: "n1", UpdatedAt: t2, Data: json.RawMessage(`{"title":"novo"}`)},
	})
	require.NoError(t, err)
	require.Equal(t, []string{"n1"}, res.Applied)
	require.Empty(t, res.Conflicts)

	// Pull a partir do cursor antigo pega a versão nova.
	pull2, err := svc.Pull(ctx, userA, pull.Cursor)
	require.NoError(t, err)
	require.Len(t, pull2.Changes, 1)
	require.JSONEq(t, `{"title":"novo"}`, string(pull2.Changes[0].Data))

	// ISOLAMENTO: o usuário B não enxerga nada do A.
	pullB, err := svc.Pull(ctx, userB, "")
	require.NoError(t, err)
	require.Empty(t, pullB.Changes)
}

func TestPushMultipleEntitiesOrdered(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	q := dbgen.New(pool)
	svc := syncpkg.New(q, pool)
	ctx := context.Background()
	user := createUser(t, q, "sub-multi")

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	_, err := svc.Push(ctx, user, []syncpkg.Change{
		{Entity: syncpkg.EntityFolder, ID: "f1", UpdatedAt: base, Data: json.RawMessage(`{"name":"Pessoal"}`)},
		{Entity: syncpkg.EntityNote, ID: "n1", UpdatedAt: base, Data: json.RawMessage(`{"title":"a"}`)},
		{Entity: syncpkg.EntitySegmentSet, ID: "s1", UpdatedAt: base, Data: json.RawMessage(`{}`)},
	})
	require.NoError(t, err)

	pull, err := svc.Pull(ctx, user, "")
	require.NoError(t, err)
	require.Len(t, pull.Changes, 3)
	// Em ordem de change_seq (ordem do push).
	require.Equal(t, "folder", pull.Changes[0].Entity)
	require.Equal(t, "note", pull.Changes[1].Entity)
	require.Equal(t, "segment_set", pull.Changes[2].Entity)
}

func createUser(t *testing.T, q *dbgen.Queries, sub string) pgtype.UUID {
	t.Helper()
	u, err := q.CreateUser(context.Background(), dbgen.CreateUserParams{
		Issuer: "https://auth.rodolfodebonis.com.br/realms/rbnotes", Subject: sub, Email: sub + "@x", Name: sub,
	})
	require.NoError(t, err)
	return u.ID
}
