// Package redis encapsula a criação do cliente Redis usado pelo scheduler (lock SET NX entre réplicas).
package redis

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"
	"github.com/redis/go-redis/v9/maintnotifications"

	"github.com/RodolfoBonis/rbnotes-api/internal/platform/config"
)

// New cria e valida um cliente Redis a partir da config.
func New(ctx context.Context, cfg config.Redis) (*redis.Client, error) {
	client := redis.NewClient(&redis.Options{
		Addr:     cfg.Addr,
		Password: cfg.Password,
		DB:       cfg.DB,
		// Notificações de manutenção exigem RESP3 e Redis 8+; desabilitamos para evitar o
		// warning de fallback contra Redis 7.
		MaintNotificationsConfig: &maintnotifications.Config{Mode: maintnotifications.ModeDisabled},
	})
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("ping redis: %w", err)
	}
	return client, nil
}
