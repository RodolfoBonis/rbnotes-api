// Package testsupport fornece utilitários compartilhados de teste (ex.: Postgres efêmero via
// testcontainers já migrado). Só é importado por arquivos _test.go, então não entra nos binários.
package testsupport

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	migrations "github.com/RodolfoBonis/rbnotes-api/db/migrations"
	"github.com/RodolfoBonis/rbnotes-api/internal/platform/db"
)

// NewPostgres sobe um Postgres efêmero, aplica as migrations e devolve um pool pronto.
// O container e o pool são encerrados automaticamente via t.Cleanup. Pula o teste em -short.
func NewPostgres(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := NewPostgresDSN(t)
	if err := db.Migrate(context.Background(), dsn, migrations.FS); err != nil {
		t.Fatalf("migrar: %v", err)
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// NewPostgresDSN sobe um Postgres efêmero SEM migrations e devolve a DSN (testes de migration).
func NewPostgresDSN(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("pula teste de integração (-short)")
	}
	ctx := context.Background()

	container, err := postgres.Run(ctx, "postgres:17-alpine",
		postgres.WithDatabase("rbnotes"),
		postgres.WithUsername("rbnotes"),
		postgres.WithPassword("rbnotes"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("subir postgres: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	return dsn
}
