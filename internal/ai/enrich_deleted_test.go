package ai

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/require"

	dbgen "github.com/RodolfoBonis/rbnotes-api/db/gen"
	"github.com/RodolfoBonis/rbnotes-api/internal/testsupport"
)

func TestStripQuote(t *testing.T) {
	raw, _ := json.Marshal(NoteAi{
		Title: "t", Tldr: "r",
		Quote:  &Quote{SegmentIndex: 0, Text: "trecho", AtMs: 10},
		Points: []string{}, Actions: []ActionItem{}, Decisions: []string{}, Questions: []string{},
	})
	out := stripQuote(raw)
	var n NoteAi
	require.NoError(t, json.Unmarshal(out, &n))
	require.Nil(t, n.Quote, "a citação é removida na nota apagada")
	require.Equal(t, "t", n.Title)
}

func TestNoteDeleted(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	q := dbgen.New(pool)
	ctx := context.Background()
	u, err := q.CreateUser(ctx, dbgen.CreateUserParams{Issuer: "i", Subject: "s-" + t.Name(), Email: "a@x", Name: "A"})
	require.NoError(t, err)
	svc := New(Deps{Queries: q, Pool: pool})
	ts := pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}

	require.NoError(t, q.UpsertNote(ctx, dbgen.UpsertNoteParams{UserID: u.ID, ID: "alive", UpdatedAt: ts, Deleted: false, Data: []byte(`{"title":"x"}`), ChangeSeq: 1}))
	require.NoError(t, q.UpsertNote(ctx, dbgen.UpsertNoteParams{UserID: u.ID, ID: "gone", UpdatedAt: ts, Deleted: true, Data: []byte(`{}`), ChangeSeq: 2}))

	del, err := svc.noteDeleted(ctx, u.ID, "alive")
	require.NoError(t, err)
	require.False(t, del)

	del, err = svc.noteDeleted(ctx, u.ID, "gone")
	require.NoError(t, err)
	require.True(t, del)

	del, err = svc.noteDeleted(ctx, u.ID, "absent")
	require.NoError(t, err)
	require.True(t, del, "nota ausente conta como apagada")
}
