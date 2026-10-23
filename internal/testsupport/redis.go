package testsupport

import (
	"context"
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// NewRedis sobe um Redis efêmero e devolve um cliente pronto (encerrados via t.Cleanup).
func NewRedis(t *testing.T) *redis.Client {
	t.Helper()
	if testing.Short() {
		t.Skip("pula teste de integração (-short)")
	}
	ctx := context.Background()
	c, err := testcontainers.Run(ctx, "redis:7-alpine",
		testcontainers.WithExposedPorts("6379/tcp"),
		testcontainers.WithWaitStrategy(wait.ForListeningPort("6379/tcp")),
	)
	if err != nil {
		t.Fatalf("subir redis: %v", err)
	}
	t.Cleanup(func() { _ = c.Terminate(context.Background()) })
	endpoint, err := c.PortEndpoint(ctx, "6379/tcp", "")
	if err != nil {
		t.Fatalf("endpoint redis: %v", err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: endpoint})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}
