package audio

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/require"

	dbgen "github.com/RodolfoBonis/rbnotes-api/db/gen"
)

// RemoveObject completa o fakeStore (definido em audio_integration_test.go) para os testes de Delete.
func (f *fakeStore) RemoveObject(_ context.Context, _, object string, _ minio.RemoveObjectOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.objects[object]; !ok {
		return minio.ErrorResponse{Code: "NoSuchKey"}
	}
	delete(f.objects, object)
	return nil
}

func (f *fakeStore) uploadOpen(uploadID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.uploads[uploadID]
	return ok
}

// TestDeleteAudioIdempotentAndAborts cobre as três situações: nada registrado, multipart em
// andamento (deve abortar) e áudio completo (deve remover o objeto); repetir é sempre 204.
func TestDeleteAudioIdempotentAndAborts(t *testing.T) {
	s, fs, _, uid := newAudioTest(t, 1<<30)
	ctx := context.Background()

	// Nada registrado → sucesso idempotente.
	require.NoError(t, s.Delete(ctx, uid, "nope"))

	// Multipart em andamento (parte 1 de 2, não concluído).
	_, err := s.PutPart(ctx, uid, "n1", 1, 2, "audio/mp4", big(minPartSize), minPartSize)
	require.NoError(t, err)
	row, err := s.q.GetAudio(ctx, dbgen.GetAudioParams{UserID: uid, NoteID: "n1"})
	require.NoError(t, err)
	require.NotEmpty(t, row.UploadID)
	require.True(t, fs.uploadOpen(row.UploadID))

	require.NoError(t, s.Delete(ctx, uid, "n1"))
	require.False(t, fs.uploadOpen(row.UploadID), "o multipart em andamento foi abortado")
	_, err = s.q.GetAudio(ctx, dbgen.GetAudioParams{UserID: uid, NoteID: "n1"})
	require.ErrorIs(t, err, pgx.ErrNoRows, "o estado do upload foi apagado")

	// Áudio completo.
	_, err = s.PutPart(ctx, uid, "n2", 1, 2, "audio/mp4", big(minPartSize), minPartSize)
	require.NoError(t, err)
	done, err := s.PutPart(ctx, uid, "n2", 2, 2, "audio/mp4", strings.NewReader("END"), 3)
	require.NoError(t, err)
	require.True(t, done)

	require.NoError(t, s.Delete(ctx, uid, "n2"))
	_, err = s.Open(ctx, uid, "n2", "")
	require.ErrorIs(t, err, ErrNotFound, "o áudio foi removido")
	require.NoError(t, s.Delete(ctx, uid, "n2"), "segundo delete é idempotente (204)")
}

// TestDeleteAudioScopedToUser prova que o Delete é escopo por usuário: apagar a mesma noteID de outro
// usuário não toca no áudio do dono.
func TestDeleteAudioScopedToUser(t *testing.T) {
	s, _, pool, uid := newAudioTest(t, 1<<30)
	ctx := context.Background()
	q := dbgen.New(pool)
	other, err := q.CreateUser(ctx, dbgen.CreateUserParams{Issuer: "i", Subject: "s-other-" + t.Name(), Email: "b@x", Name: "B"})
	require.NoError(t, err)

	_, err = s.PutPart(ctx, uid, "n1", 1, 2, "audio/mp4", big(minPartSize), minPartSize)
	require.NoError(t, err)
	done, err := s.PutPart(ctx, uid, "n1", 2, 2, "audio/mp4", strings.NewReader("END"), 3)
	require.NoError(t, err)
	require.True(t, done)

	require.NoError(t, s.Delete(ctx, other.ID, "n1"), "delete de outro usuário para a mesma noteID")
	c, err := s.Open(ctx, uid, "n1", "")
	require.NoError(t, err, "o áudio do dono continua lá")
	_ = c.Body.Close()
}
