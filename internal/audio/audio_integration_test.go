package audio

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/require"

	dbgen "github.com/RodolfoBonis/rbnotes-api/db/gen"
	"github.com/RodolfoBonis/rbnotes-api/internal/testsupport"
)

// fakeStore simula o multipart do MinIO em memória.
type fakeStore struct {
	mu      sync.Mutex
	nextID  int
	uploads map[string]map[int][]byte
	objects map[string][]byte
}

func newFakeStore() *fakeStore {
	return &fakeStore{uploads: map[string]map[int][]byte{}, objects: map[string][]byte{}}
}

func (f *fakeStore) BucketExists(context.Context, string) (bool, error) { return true, nil }
func (f *fakeStore) MakeBucket(context.Context, string, minio.MakeBucketOptions) error {
	return nil
}

func (f *fakeStore) NewMultipartUpload(_ context.Context, _, _ string, _ minio.PutObjectOptions) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := "up-" + strconv.Itoa(f.nextID)
	f.uploads[id] = map[int][]byte{}
	return id, nil
}

func (f *fakeStore) PutObjectPart(_ context.Context, _, _, uploadID string, part int, data io.Reader, size int64, _ minio.PutObjectPartOptions) (minio.ObjectPart, error) {
	b, _ := io.ReadAll(data)
	f.mu.Lock()
	defer f.mu.Unlock()
	parts, ok := f.uploads[uploadID]
	if !ok {
		return minio.ObjectPart{}, minio.ErrorResponse{Code: "NoSuchUpload", Message: "upload sumiu"}
	}
	parts[part] = b
	return minio.ObjectPart{PartNumber: part, ETag: fmt.Sprintf("etag-%s-%d", uploadID, part), Size: size}, nil
}

func (f *fakeStore) CompleteMultipartUpload(_ context.Context, _, object, uploadID string, parts []minio.CompletePart, _ minio.PutObjectOptions) (minio.UploadInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	stored, ok := f.uploads[uploadID]
	if !ok {
		return minio.UploadInfo{}, minio.ErrorResponse{Code: "NoSuchUpload"}
	}
	var buf bytes.Buffer
	for _, p := range parts {
		buf.Write(stored[p.PartNumber])
	}
	f.objects[object] = buf.Bytes()
	delete(f.uploads, uploadID)
	return minio.UploadInfo{Size: int64(buf.Len())}, nil
}

func (f *fakeStore) AbortMultipartUpload(_ context.Context, _, _, uploadID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.uploads, uploadID)
	return nil
}

func (f *fakeStore) GetObject(_ context.Context, _, object string, opts minio.GetObjectOptions) (io.ReadCloser, minio.ObjectInfo, http.Header, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.objects[object]
	if !ok {
		return nil, minio.ObjectInfo{}, nil, minio.ErrorResponse{Code: "NoSuchKey"}
	}
	if rng := opts.Header().Get("Range"); rng != "" {
		var start, end int64
		if _, err := fmt.Sscanf(rng, "bytes=%d-%d", &start, &end); err == nil {
			data = data[start : end+1]
		}
	}
	return io.NopCloser(bytes.NewReader(data)), minio.ObjectInfo{Size: int64(len(data))}, nil, nil
}

func (f *fakeStore) expire(uploadID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.uploads, uploadID)
}

func newAudioTest(t *testing.T, maxBytes int64) (*Service, *fakeStore, *pgxpool.Pool, pgtype.UUID) {
	t.Helper()
	pool := testsupport.NewPostgres(t)
	q := dbgen.New(pool)
	u, err := q.CreateUser(context.Background(), dbgen.CreateUserParams{Issuer: "i", Subject: "s-" + t.Name(), Email: "a@x", Name: "A"})
	require.NoError(t, err)
	fs := newFakeStore()
	s := &Service{store: fs, bucket: "b", q: q, pool: pool, maxBytes: maxBytes, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	return s, fs, pool, u.ID
}

func big(n int) io.Reader { return bytes.NewReader(bytes.Repeat([]byte("a"), n)) }

func TestPutPartHappyPathAndDownload(t *testing.T) {
	s, _, _, uid := newAudioTest(t, 1<<30)
	ctx := context.Background()

	// Parte 1 (não final) ≥ 5 MiB, parte 2 pequena (final).
	done, err := s.PutPart(ctx, uid, "n1", 1, 2, "audio/mp4", big(minPartSize), minPartSize)
	require.NoError(t, err)
	require.False(t, done)
	done, err = s.PutPart(ctx, uid, "n1", 2, 2, "audio/mp4", strings.NewReader("END"), 3)
	require.NoError(t, err)
	require.True(t, done)

	// Download completo.
	c, err := s.Open(ctx, uid, "n1", "")
	require.NoError(t, err)
	defer func() { _ = c.Body.Close() }()
	require.False(t, c.Partial)
	require.Equal(t, int64(minPartSize+3), c.Size)
	body, _ := io.ReadAll(c.Body)
	require.Equal(t, minPartSize+3, len(body))
	require.True(t, strings.HasSuffix(string(body), "END"))

	// Download com Range (últimos 3 bytes).
	rc, err := s.Open(ctx, uid, "n1", "bytes=-3")
	require.NoError(t, err)
	defer func() { _ = rc.Body.Close() }()
	require.True(t, rc.Partial)
	require.Equal(t, int64(3), rc.Length())
	rb, _ := io.ReadAll(rc.Body)
	require.Equal(t, "END", string(rb))
}

func TestPutPartNoUploadForLaterPart(t *testing.T) {
	s, _, _, uid := newAudioTest(t, 1<<30)
	_, err := s.PutPart(context.Background(), uid, "n1", 2, 2, "audio/mp4", strings.NewReader("x"), 1)
	require.ErrorIs(t, err, ErrNoUpload)
}

func TestPutPartTotalMismatch(t *testing.T) {
	s, _, _, uid := newAudioTest(t, 1<<30)
	ctx := context.Background()
	_, err := s.PutPart(ctx, uid, "n1", 1, 3, "audio/mp4", big(minPartSize), minPartSize)
	require.NoError(t, err)
	_, err = s.PutPart(ctx, uid, "n1", 2, 2, "audio/mp4", big(minPartSize), minPartSize)
	require.ErrorIs(t, err, ErrTotalMismatch)
}

func TestPutPartIdempotentLastAndConflict(t *testing.T) {
	s, _, _, uid := newAudioTest(t, 1<<30)
	ctx := context.Background()
	_, err := s.PutPart(ctx, uid, "n1", 1, 3, "audio/mp4", big(minPartSize), minPartSize)
	require.NoError(t, err)
	_, err = s.PutPart(ctx, uid, "n1", 2, 3, "audio/mp4", big(minPartSize), minPartSize)
	require.NoError(t, err)
	done, err := s.PutPart(ctx, uid, "n1", 3, 3, "audio/mp4", strings.NewReader("Z"), 1)
	require.NoError(t, err)
	require.True(t, done)

	// Reenvio da última parte depois de completo → sucesso idempotente.
	done, err = s.PutPart(ctx, uid, "n1", 3, 3, "audio/mp4", strings.NewReader("Z"), 1)
	require.NoError(t, err)
	require.True(t, done)

	// Parte do meio depois de completo → conflito.
	_, err = s.PutPart(ctx, uid, "n1", 2, 3, "audio/mp4", big(minPartSize), minPartSize)
	require.ErrorIs(t, err, ErrAlreadyCompleted)
}

func TestPutPartMaxBytes(t *testing.T) {
	s, _, _, uid := newAudioTest(t, 10) // teto de 10 bytes
	_, err := s.PutPart(context.Background(), uid, "n1", 1, 1, "audio/mp4", strings.NewReader("12345678901234567890"), 20)
	require.ErrorIs(t, err, ErrTooLarge)
}

func TestPutPartNoSuchUploadRestarts(t *testing.T) {
	s, fs, _, uid := newAudioTest(t, 1<<30)
	ctx := context.Background()
	_, err := s.PutPart(ctx, uid, "n1", 1, 2, "audio/mp4", big(minPartSize), minPartSize)
	require.NoError(t, err)

	// Simula o multipart expirando no MinIO.
	row, err := s.q.GetAudio(ctx, dbgen.GetAudioParams{UserID: uid, NoteID: "n1"})
	require.NoError(t, err)
	oldUpload := row.UploadID
	fs.expire(oldUpload)

	_, err = s.PutPart(ctx, uid, "n1", 2, 2, "audio/mp4", big(minPartSize), minPartSize)
	require.ErrorIs(t, err, ErrNoUpload, "NoSuchUpload deve reiniciar e pedir reenvio da parte 1")

	row, err = s.q.GetAudio(ctx, dbgen.GetAudioParams{UserID: uid, NoteID: "n1"})
	require.NoError(t, err)
	require.NotEqual(t, oldUpload, row.UploadID, "reiniciou com um novo upload_id")
	require.False(t, row.Completed)
}
