// Package migrations embute os arquivos .sql de migração para que o binário aplique
// o schema no startup, sem depender de ferramentas externas na imagem (distroless).
package migrations

import "embed"

// FS contém todas as migrations goose, ordenadas por prefixo numérico.
//
//go:embed *.sql
var FS embed.FS
