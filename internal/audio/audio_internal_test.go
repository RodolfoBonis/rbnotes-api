package audio

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/require"
)

func TestMergeAndDecodeParts(t *testing.T) {
	b, err := mergePart(nil, 1, "etag-1")
	require.NoError(t, err)
	b, err = mergePart(b, 3, "etag-3")
	require.NoError(t, err)
	b, err = mergePart(b, 2, "etag-2")
	require.NoError(t, err)
	// Reenvio da mesma parte sobrescreve o ETag (idempotente).
	b, err = mergePart(b, 2, "etag-2b")
	require.NoError(t, err)

	m, err := decodeParts(b)
	require.NoError(t, err)
	require.Equal(t, map[int]string{1: "etag-1", 2: "etag-2b", 3: "etag-3"}, m)
}

func TestDecodePartsEmpty(t *testing.T) {
	m, err := decodeParts(nil)
	require.NoError(t, err)
	require.Empty(t, m)
}

func TestObjectKeyScopedByUser(t *testing.T) {
	require.Equal(t, "users/u1/notes/n1.m4a", objectKey("u1", "n1"))
}

func TestNormalizeContentType(t *testing.T) {
	ct, err := normalizeContentType("")
	require.NoError(t, err)
	require.Equal(t, "audio/mp4", ct)

	ct, err = normalizeContentType("application/octet-stream")
	require.NoError(t, err)
	require.Equal(t, "audio/mp4", ct)

	ct, err = normalizeContentType("audio/mpeg; charset=binary")
	require.NoError(t, err)
	require.Equal(t, "audio/mpeg", ct)

	_, err = normalizeContentType("text/html")
	require.ErrorIs(t, err, ErrBadContentType)
}

func TestParseRange(t *testing.T) {
	_, _, partial, err := parseRange("", 100)
	require.NoError(t, err)
	require.False(t, partial)

	s, e, partial, err := parseRange("bytes=0-49", 100)
	require.NoError(t, err)
	require.True(t, partial)
	require.Equal(t, int64(0), s)
	require.Equal(t, int64(49), e)

	s, e, _, err = parseRange("bytes=10-", 100)
	require.NoError(t, err)
	require.Equal(t, int64(10), s)
	require.Equal(t, int64(99), e)

	s, e, _, err = parseRange("bytes=-20", 100)
	require.NoError(t, err)
	require.Equal(t, int64(80), s)
	require.Equal(t, int64(99), e)

	// Clampa o fim ao tamanho do objeto.
	_, e, _, err = parseRange("bytes=0-999", 100)
	require.NoError(t, err)
	require.Equal(t, int64(99), e)

	for _, bad := range []string{"items=0-1", "bytes=100-200", "bytes=50-10", "bytes=abc-"} {
		_, _, _, err := parseRange(bad, 100)
		require.ErrorIs(t, err, ErrBadRange, bad)
	}
}

// TestPutPartValidationPreTx cobre as rejeições que acontecem ANTES de abrir a transação (não tocam
// o banco nem o store): bounds, Content-Type e tamanho mínimo de parte não final.
func TestPutPartValidationPreTx(t *testing.T) {
	s := &Service{maxBytes: 1 << 30}
	ctx := context.Background()
	var uid pgtype.UUID

	_, err := s.PutPart(ctx, uid, "n1", 2, 1, "audio/mp4", strings.NewReader(""), 10)
	require.ErrorIs(t, err, ErrBadPart)

	_, err = s.PutPart(ctx, uid, "n1", 1, 1, "text/html", strings.NewReader(""), 10)
	require.ErrorIs(t, err, ErrBadContentType)

	_, err = s.PutPart(ctx, uid, "n1", 1, 2, "audio/mp4", strings.NewReader(""), 1000)
	require.ErrorIs(t, err, ErrPartTooSmall)
}
