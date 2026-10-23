package audio

import (
	"testing"

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
