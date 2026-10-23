package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

func TestLabelsPreinitialized(t *testing.T) {
	// As séries de IA nascem em 0 (inclusive rate_limited), para alertas de ausência funcionarem.
	require.Equal(t, 0.0, testutil.ToFloat64(aiJobs.WithLabelValues("ask", "rate_limited")))
	require.Equal(t, 0.0, testutil.ToFloat64(jobsProcessed.WithLabelValues("ai_job", "interrupted")))
	// Dependências começam disponíveis (1).
	require.Equal(t, 1.0, testutil.ToFloat64(dependencyUp.WithLabelValues("redis")))
}

func TestSetDependencyUp(t *testing.T) {
	SetDependencyUp("minio", false)
	require.Equal(t, 0.0, testutil.ToFloat64(dependencyUp.WithLabelValues("minio")))
	SetDependencyUp("minio", true)
	require.Equal(t, 1.0, testutil.ToFloat64(dependencyUp.WithLabelValues("minio")))
}
