package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadDefaultsAndValidation(t *testing.T) {
	// OIDC exige issuer.
	t.Setenv("DATABASE_URL", "postgres://x/y")
	t.Setenv("AUTH_MODE", "oidc")
	t.Setenv("AUTH_ISSUER", "")
	_, err := Load()
	require.Error(t, err)

	t.Setenv("AUTH_ISSUER", "https://auth.rodolfodebonis.com.br/realms/rbnotes")
	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, "rbnotes-api", cfg.Auth.Audience)
	require.Equal(t, int64(52428800), cfg.Audio.ChunkLimit)
	require.Equal(t, "sonnet", cfg.AI.Model)
}

func TestDevModeOnlyInDev(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x/y")
	t.Setenv("AUTH_MODE", "dev")
	t.Setenv("AUTH_ISSUER", "")
	t.Setenv("APP_ENV", "prod")
	_, err := Load()
	require.Error(t, err, "dev bypass não pode valer em prod")

	t.Setenv("APP_ENV", "dev")
	_, err = Load()
	require.NoError(t, err)
}

func TestRejectsBadModel(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x/y")
	t.Setenv("AUTH_MODE", "dev")
	t.Setenv("APP_ENV", "dev")
	t.Setenv("AUTH_ISSUER", "")
	t.Setenv("AI_MODEL", "gpt")
	_, err := Load()
	require.Error(t, err)
}
