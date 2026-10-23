// Package rbnotes expõe metadados do build (versão) embutidos a partir de version.txt.
package rbnotes

import (
	_ "embed"
	"strings"
)

//go:embed version.txt
var versionRaw string

// Version é a versão da aplicação (conteúdo de version.txt).
func Version() string { return strings.TrimSpace(versionRaw) }
