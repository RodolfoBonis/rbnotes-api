package api

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLastEventIDRe(t *testing.T) {
	for _, ok := range []string{"0-0", "1760000000000-0", "123-45"} {
		require.True(t, lastEventIDRe.MatchString(ok), ok)
	}
	for _, bad := range []string{"", "0", "abc", "1-", "-1", "1-2-3", "9 9-0"} {
		require.False(t, lastEventIDRe.MatchString(bad), bad)
	}
}
