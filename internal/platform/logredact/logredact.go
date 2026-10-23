// Package logredact fornece a redação de segredos nos logs estruturados (slog).
//
// A defesa tem duas camadas:
//   - ReplaceAttr: qualquer atributo cuja CHAVE case com secretKeyRe tem o valor trocado por
//     "[REDACTED]", em qualquer nível de aninhamento (grupos).
//   - Nunca despejar structs de credencial: os tipos que carregam segredos devem implementar
//     slog.LogValuer (ver Redact) e/ou fmt.Stringer para redigir a si mesmos.
package logredact

import (
	"log/slog"
	"regexp"
)

// secretKeyRe casa (case-insensitive) chaves de atributo que nunca devem ter o valor logado.
var secretKeyRe = regexp.MustCompile(`(?i)(npsso|token|secret|key|authorization|password)`)

// placeholder é o valor que substitui qualquer segredo nos logs.
const placeholder = "[REDACTED]"

// ReplaceAttr é o slog.HandlerOptions.ReplaceAttr que redige valores de chaves sensíveis.
//
// Observação sobre falsos positivos: chaves "benignas" que contêm "key" (ex.: "dedupe_key",
// "idempotency_key") também são redigidas. É uma troca consciente: preferimos redigir demais a
// vazar um segredo. Logue identificadores não-sensíveis com nomes que não casem o regex (ex.: "id").
func ReplaceAttr(_ []string, a slog.Attr) slog.Attr {
	if secretKeyRe.MatchString(a.Key) {
		return slog.String(a.Key, placeholder)
	}
	return a
}

// Redact é um valor slog.LogValuer que sempre loga "[REDACTED]". Embuta-o (ou retorne-o de
// LogValue) em structs de credencial para garantir que nunca vazem, mesmo via slog.Any.
type Redact struct{}

// LogValue implementa slog.LogValuer.
func (Redact) LogValue() slog.Value { return slog.StringValue(placeholder) }

// String implementa fmt.Stringer, cobrindo também fmt/%v e %s.
func (Redact) String() string { return placeholder }
