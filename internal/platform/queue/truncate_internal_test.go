package queue

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

// TestTruncateRunesUTF8Safe garante que o corte não parte um code point UTF-8 no meio (finding 16).
func TestTruncateRunesUTF8Safe(t *testing.T) {
	// "ç" ocupa 2 bytes; "😀" ocupa 4. Uma string só de multibyte força cortes no limiar.
	s := strings.Repeat("ç", 10) + strings.Repeat("😀", 10)
	for n := 0; n <= len(s); n++ {
		out := truncateRunes(s, n)
		require.LessOrEqual(t, len(out), n)
		require.True(t, utf8.ValidString(out), "corte em %d bytes produziu UTF-8 inválido: %q", n, out)
	}

	// ASCII puro corta exatamente em n.
	ascii := strings.Repeat("a", 100)
	require.Len(t, truncateRunes(ascii, 10), 10)

	// String menor que n é devolvida intacta.
	require.Equal(t, "oi", truncateRunes("oi", 50))
}
