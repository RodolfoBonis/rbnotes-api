package sync

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseCursor(t *testing.T) {
	n, err := parseCursor("")
	require.NoError(t, err)
	require.Equal(t, int64(0), n)

	n, err = parseCursor("42")
	require.NoError(t, err)
	require.Equal(t, int64(42), n)

	for _, bad := range []string{"abc", "-1", "1.5", "0x10"} {
		_, err := parseCursor(bad)
		require.ErrorIs(t, err, ErrBadCursor, bad)
	}
}
