package db

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"

	_ "github.com/jackc/pgx/v5/stdlib" // driver database/sql para o goose
	"github.com/pressly/goose/v3"
)

// migrateLockKey é uma chave fixa de advisory lock do Postgres. Réplicas concorrentes
// (durante um rolling update) serializam a migração: a segunda espera a primeira terminar.
const migrateLockKey int64 = 0x72626e6f746573 // "rbnotes" em hex

// Migrate aplica as migrations embutidas usando um advisory lock para ser seguro com múltiplas
// réplicas. É idempotente: se o schema já está atualizado, não faz nada.
func Migrate(ctx context.Context, url string, fsys fs.FS) error {
	sqlDB, err := sql.Open("pgx", url)
	if err != nil {
		return fmt.Errorf("abrir conexão para migração: %w", err)
	}
	defer func() { _ = sqlDB.Close() }()

	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return fmt.Errorf("obter conexão para lock: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", migrateLockKey); err != nil {
		return fmt.Errorf("adquirir advisory lock de migração: %w", err)
	}
	defer func() {
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", migrateLockKey)
	}()

	// NOTA: goose.SetBaseFS/SetDialect mutam estado GLOBAL do goose. É seguro aqui porque Migrate
	// roda uma única vez, sequencialmente, no startup (antes de abrir o pool e servir tráfego).
	goose.SetBaseFS(fsys)
	defer goose.SetBaseFS(nil)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("definir dialeto goose: %w", err)
	}
	if err := goose.UpContext(ctx, sqlDB, "."); err != nil {
		return fmt.Errorf("aplicar migrations: %w", err)
	}
	return nil
}
