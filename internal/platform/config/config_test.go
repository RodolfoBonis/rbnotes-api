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

func TestAllowedEmailsNormalizedAndRequiredInProd(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x/y")
	t.Setenv("AUTH_MODE", "oidc")
	t.Setenv("AUTH_ISSUER", "https://auth.example/realms/rbnotes")

	// Dev: lista vazia é aceita.
	t.Setenv("APP_ENV", "dev")
	cfg, err := Load()
	require.NoError(t, err)
	require.Empty(t, cfg.Auth.AllowedEmails)

	// Prod + oidc sem lista: fail fast.
	t.Setenv("APP_ENV", "prod")
	_, err = Load()
	require.ErrorContains(t, err, "AUTH_ALLOWED_EMAILS")
	t.Setenv("AUTH_ALLOWED_EMAILS", " , ")
	_, err = Load()
	require.ErrorContains(t, err, "AUTH_ALLOWED_EMAILS")

	// Prod com lista: normaliza trim/lowercase/dedupe.
	t.Setenv("AUTH_ALLOWED_EMAILS", " Dev@Rodolfodebonis.com.br, ,outro@x.com,dev@rodolfodebonis.com.br")
	cfg, err = Load()
	require.NoError(t, err)
	require.Equal(t, []string{"dev@rodolfodebonis.com.br", "outro@x.com"}, cfg.Auth.AllowedEmails)
}

func TestMinIOEndpoint(t *testing.T) {
	tests := []struct {
		name       string
		m          MinIO
		wantHost   string
		wantSecure bool
	}{
		{"host:port", MinIO{Server: "minio.minio.svc.cluster.local:9000"}, "minio.minio.svc.cluster.local:9000", false},
		{"host:port com SSL", MinIO{Server: "minio:9000", UseSSL: true}, "minio:9000", true},
		{"http://", MinIO{Server: "http://minio:9000/", UseSSL: true}, "minio:9000", false},
		{"https://", MinIO{Server: "HTTPS://minio.example.com"}, "minio.example.com", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			host, secure := tc.m.Endpoint()
			require.Equal(t, tc.wantHost, host)
			require.Equal(t, tc.wantSecure, secure)
		})
	}
}

func TestMinIOEnvNames(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x/y")
	t.Setenv("AUTH_MODE", "dev")
	t.Setenv("APP_ENV", "dev")
	t.Setenv("AUTH_ISSUER", "")
	t.Setenv("MINIO_SERVER", "minio:9000")
	t.Setenv("MINIO_ACCESS_ID", "id")
	t.Setenv("MINIO_SECRET_KEY", "secret")
	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, "minio:9000", cfg.MinIO.Server)
	require.Equal(t, "id", cfg.MinIO.AccessID)
	require.Equal(t, "secret", cfg.MinIO.SecretKey)
}
