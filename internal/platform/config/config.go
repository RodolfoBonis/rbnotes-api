// Package config carrega a configuração do serviço a partir do ambiente.
//
// O carregamento fica atrás de Load() para que, no futuro, a fonte possa passar a ser o Vault/SSM
// sem alterar os consumidores. Campos obrigatórios sem default fazem Load() falhar com erro claro.
// Segredos NUNCA são embutidos em mensagens de erro.
package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
)

// AuthMode define como os tokens são validados.
type AuthMode string

const (
	// AuthModeDev confia no header de debug e NÃO valida assinatura. Apenas dev local.
	AuthModeDev AuthMode = "dev"
	// AuthModeOIDC valida o JWT (Bearer) contra o JWKS do issuer (realm Keycloak).
	AuthModeOIDC AuthMode = "oidc"
)

// Config agrega toda a configuração do serviço (API + ai-worker).
type Config struct {
	Env       string `env:"APP_ENV" envDefault:"dev"`
	HTTP      HTTP
	Metrics   Metrics
	DB        DB
	Redis     Redis
	Auth      Auth
	Telemetry Telemetry
	MinIO     MinIO
	AI        AI
	Audio     Audio
}

// HTTP configura o servidor HTTP público da API.
type HTTP struct {
	Addr            string        `env:"HTTP_ADDR" envDefault:":8080"`
	ReadTimeout     time.Duration `env:"HTTP_READ_TIMEOUT" envDefault:"15s"`
	WriteTimeout    time.Duration `env:"HTTP_WRITE_TIMEOUT" envDefault:"30s"`
	ShutdownTimeout time.Duration `env:"HTTP_SHUTDOWN_TIMEOUT" envDefault:"15s"`
	// CORSAllowedOrigins são as origens (scheme://host[:port]) autorizadas a chamar a API pelo
	// desktop/web. A API fica em subdomínio próprio (api.notes.<domínio>).
	CORSAllowedOrigins []string `env:"CORS_ALLOWED_ORIGINS" envSeparator:","`
}

// Metrics configura o endpoint INTERNO de métricas (/metrics + /healthz). NUNCA exposto no host
// público: Prometheus raspa via ServiceMonitor no cluster.
type Metrics struct {
	Addr string `env:"METRICS_ADDR" envDefault:":9090"`
}

// DB configura a conexão com o Postgres.
type DB struct {
	URL            string `env:"DATABASE_URL,required"`
	MaxConns       int32  `env:"DB_MAX_CONNS" envDefault:"20"`
	MinConns       int32  `env:"DB_MIN_CONNS" envDefault:"2"`
	MigrateOnStart bool   `env:"DB_MIGRATE_ON_START" envDefault:"true"`
}

// Redis configura o cliente Redis (canal da resposta da IA em tempo real — SSE).
type Redis struct {
	Addr     string `env:"REDIS_ADDR" envDefault:"localhost:6379"`
	Password string `env:"REDIS_PASSWORD" envDefault:""`
	DB       int    `env:"REDIS_DB" envDefault:"0"`
}

// Auth configura a validação dos tokens (Keycloak OIDC/JWT do realm rbnotes).
//
//   - Mode "oidc" (default) valida o JWT contra o JWKS do Issuer, conferindo iss, aud (Audience) e
//     a role exigida em realm_access.roles (quando RequiredRole não for vazio).
//   - Mode "dev" confia no header X-Debug-User (dev local, sem Keycloak). Aceito SOMENTE com
//     APP_ENV=dev e AUTH_ISSUER vazio — nunca escorrega para produção por engano.
type Auth struct {
	Mode     AuthMode `env:"AUTH_MODE" envDefault:"oidc"`
	Issuer   string   `env:"AUTH_ISSUER" envDefault:""`
	Audience string   `env:"AUTH_AUDIENCE" envDefault:"rbnotes-api"`
	// RequiredRole: realm role exigida no token. Vazio = não exige (o acesso é a própria conta).
	RequiredRole string `env:"AUTH_REQUIRED_ROLE"`
	// AllowedEmails é a allowlist de e-mails (AUTH_ALLOWED_EMAILS, separados por vírgula, normalizados
	// para lowercase/trim). Não vazia: só tokens com email_verified=true e e-mail na lista passam
	// (403 not_allowed); evita que qualquer conta brokered (ex.: Google) vire usuário. Obrigatória
	// com AUTH_MODE=oidc em APP_ENV=prod.
	AllowedEmails []string `env:"AUTH_ALLOWED_EMAILS" envSeparator:","`
}

// Telemetry configura logs e OpenTelemetry (exporter desligado por default).
type Telemetry struct {
	OTelEnabled  bool   `env:"OTEL_ENABLED" envDefault:"false"`
	ServiceName  string `env:"OTEL_SERVICE_NAME" envDefault:"rbnotes-api"`
	OTLPEndpoint string `env:"OTEL_EXPORTER_OTLP_ENDPOINT" envDefault:"localhost:4317"`
	LogLevel     string `env:"LOG_LEVEL" envDefault:"info"`
}

// MinIO configura o armazenamento de áudio (S3-compatível interno ao cluster). As credenciais são
// SEGREDO (Vault k3s/rbnotes/prod/minio); nunca são logadas.
type MinIO struct {
	// Server é o MINIO_SERVER do secret do Vault (host:port; um esquema http(s):// é tolerado — ver
	// Endpoint).
	Server    string `env:"MINIO_SERVER" envDefault:"localhost:9000"`
	AccessID  string `env:"MINIO_ACCESS_ID"`
	SecretKey string `env:"MINIO_SECRET_KEY"`
	Bucket    string `env:"MINIO_BUCKET" envDefault:"rbnotes-audio"`
	UseSSL    bool   `env:"MINIO_USE_SSL" envDefault:"false"`
	Region    string `env:"MINIO_REGION" envDefault:"us-east-1"`
}

// Endpoint devolve o host[:port] para o cliente minio-go e se deve usar TLS. Um prefixo
// http:// ou https:// em Server é removido e define o SSL (https => true); sem esquema vale UseSSL.
func (m MinIO) Endpoint() (hostPort string, secure bool) {
	server := strings.TrimSpace(m.Server)
	lower := strings.ToLower(server)
	switch {
	case strings.HasPrefix(lower, "https://"):
		return strings.TrimRight(server[len("https://"):], "/"), true
	case strings.HasPrefix(lower, "http://"):
		return strings.TrimRight(server[len("http://"):], "/"), false
	default:
		return strings.TrimRight(server, "/"), m.UseSSL
	}
}

// Audio configura os limites do upload em chunks.
type Audio struct {
	// ChunkLimit: tamanho máximo (e padrão recomendado) de um chunk. 16 MiB equilibra o mínimo de
	// 5 MiB exigido pelo S3 em partes não finais e o teto de latência por requisição.
	ChunkLimit int64 `env:"AUDIO_CHUNK_LIMIT" envDefault:"16777216"` // 16 MiB
	// MaxBytes: teto do áudio completo de uma nota (soma das partes). Default 2 GiB.
	MaxBytes int64 `env:"AUDIO_MAX_BYTES" envDefault:"2147483648"` // 2 GiB
}

// AI configura o ai-worker (CLI do Claude Code headless). A API não executa a CLI: só enfileira e
// repassa a stream. OAuthToken é o CLAUDE_CODE_OAUTH_TOKEN da assinatura (SEGREDO: só vai para o
// processo da CLI, nunca é logado) e só é exigido pelo ai-worker.
type AI struct {
	ClaudeBin  string `env:"AI_CLAUDE_BIN" envDefault:"claude"`
	OAuthToken string `env:"CLAUDE_CODE_OAUTH_TOKEN"`
	// Model é o modelo padrão (alias curto). A whitelist de modelos permitidos é AllowedModels.
	Model string `env:"AI_MODEL" envDefault:"sonnet"`
	// DailyLimit: pedidos de IA por usuário em 24 h (0 = sem teto).
	DailyLimit int           `env:"AI_DAILY_LIMIT" envDefault:"50"`
	JobTimeout time.Duration `env:"AI_JOB_TIMEOUT" envDefault:"6m"`
	// Home é o HOME da CLI (~/.claude guarda as sessões do --resume do chat).
	Home string `env:"AI_HOME" envDefault:"/home/node"`
	// Workdir é a raiz dos cwd da CLI (um por conversa: o --resume exige o mesmo cwd).
	Workdir string `env:"AI_WORKDIR" envDefault:"/home/node/work"`
	// Retention é por quanto tempo guardar ai_requests e o input/result dos ai_jobs, e os jobs
	// terminais da fila, antes de o housekeeping limpá-los. Default 30 dias.
	Retention time.Duration `env:"AI_RETENTION" envDefault:"720h"`
}

// AllowedModels é a whitelist de modelos aceitos nos pedidos de IA (alias curtos da CLI).
var AllowedModels = map[string]bool{"sonnet": true, "opus": true, "haiku": true}

// Load lê a configuração do ambiente e valida campos obrigatórios.
func Load() (Config, error) {
	var cfg Config
	if err := env.Parse(&cfg); err != nil {
		return Config{}, fmt.Errorf("carregar config: %w", err)
	}
	cfg.Auth.AllowedEmails = normalizeEmails(cfg.Auth.AllowedEmails)
	if err := validateAuth(cfg); err != nil {
		return Config{}, err
	}
	if cfg.AI.Model != "" && !AllowedModels[cfg.AI.Model] {
		return Config{}, fmt.Errorf("carregar config: AI_MODEL inválido %q (use sonnet, opus ou haiku)", cfg.AI.Model)
	}
	if cfg.Audio.ChunkLimit <= 0 {
		return Config{}, fmt.Errorf("carregar config: AUDIO_CHUNK_LIMIT deve ser > 0")
	}
	if cfg.Audio.MaxBytes <= 0 {
		return Config{}, fmt.Errorf("carregar config: AUDIO_MAX_BYTES deve ser > 0")
	}
	return cfg, nil
}

// validateAuth aplica a política de segurança do modo de autenticação (ver type Auth).
func validateAuth(cfg Config) error {
	switch cfg.Auth.Mode {
	case AuthModeOIDC:
		if cfg.Auth.Issuer == "" {
			return fmt.Errorf("carregar config: AUTH_ISSUER é obrigatório quando AUTH_MODE=oidc")
		}
		if cfg.Auth.Audience == "" {
			return fmt.Errorf("carregar config: AUTH_AUDIENCE é obrigatório quando AUTH_MODE=oidc")
		}
		if cfg.IsProd() && len(cfg.Auth.AllowedEmails) == 0 {
			return fmt.Errorf("carregar config: AUTH_ALLOWED_EMAILS é obrigatório com AUTH_MODE=oidc em APP_ENV=prod " +
				"(sem allowlist, qualquer conta do realm — inclusive Google — seria aceita)")
		}
		return nil
	case AuthModeDev:
		if cfg.Env != "dev" {
			return fmt.Errorf("carregar config: AUTH_MODE=dev só é permitido com APP_ENV=dev (APP_ENV=%q)", cfg.Env)
		}
		if cfg.Auth.Issuer != "" {
			return fmt.Errorf("carregar config: AUTH_MODE=dev exige AUTH_ISSUER vazio (há um issuer configurado — use AUTH_MODE=oidc)")
		}
		return nil
	default:
		return fmt.Errorf("carregar config: AUTH_MODE inválido %q (use oidc ou dev)", cfg.Auth.Mode)
	}
}

// IsProd indica se o serviço roda em produção.
func (c Config) IsProd() bool { return c.Env == "prod" }

// normalizeEmails aplica trim/lowercase e descarta entradas vazias e duplicadas.
func normalizeEmails(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, e := range in {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" || seen[e] {
			continue
		}
		seen[e] = true
		out = append(out, e)
	}
	return out
}
