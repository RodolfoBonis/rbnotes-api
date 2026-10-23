// Package httperr define erros de aplicação tipados e sua serialização no formato
// application/problem+json (RFC 7807), com trace_id do span ativo quando houver.
package httperr

import (
	"encoding/json"
	"errors"
	"net/http"

	"go.opentelemetry.io/otel/trace"
)

// typeBase é o prefixo dos campos `type` (URI estável dos erros do serviço).
const typeBase = "https://errors.notes.rb.lab/"

// Kind classifica o erro e determina o status HTTP e o sufixo do `type`.
type Kind int

// Tipos de erro suportados.
const (
	KindInternal Kind = iota
	KindBadRequest
	KindValidation
	KindUnauthorized
	KindForbidden
	KindNotFound
	KindConflict
	KindRateLimited
	// KindBadGateway sinaliza falha de um provider externo. Status 502 — o cliente pode re-tentar.
	KindBadGateway
	// KindNotImplemented sinaliza um recurso opcional não configurado no deployment. Status 501.
	KindNotImplemented
	// KindUnavailable sinaliza indisponibilidade temporária (ex.: provider OIDC ainda carregando).
	// Status 503 — o cliente pode re-tentar em instantes.
	KindUnavailable
	// KindPayloadTooLarge sinaliza corpo acima do limite aceito. Status 413.
	KindPayloadTooLarge
	// KindRangeNotSatisfiable sinaliza um header Range que não cabe no recurso. Status 416.
	KindRangeNotSatisfiable
)

func (k Kind) status() int {
	switch k {
	case KindBadRequest:
		return http.StatusBadRequest
	case KindValidation:
		return http.StatusUnprocessableEntity
	case KindUnauthorized:
		return http.StatusUnauthorized
	case KindForbidden:
		return http.StatusForbidden
	case KindNotFound:
		return http.StatusNotFound
	case KindConflict:
		return http.StatusConflict
	case KindRateLimited:
		return http.StatusTooManyRequests
	case KindBadGateway:
		return http.StatusBadGateway
	case KindNotImplemented:
		return http.StatusNotImplemented
	case KindUnavailable:
		return http.StatusServiceUnavailable
	case KindPayloadTooLarge:
		return http.StatusRequestEntityTooLarge
	case KindRangeNotSatisfiable:
		return http.StatusRequestedRangeNotSatisfiable
	default:
		return http.StatusInternalServerError
	}
}

func (k Kind) slug() string {
	switch k {
	case KindBadRequest:
		return "bad-request"
	case KindValidation:
		return "validation"
	case KindUnauthorized:
		return "unauthorized"
	case KindForbidden:
		return "forbidden"
	case KindNotFound:
		return "not-found"
	case KindConflict:
		return "conflict"
	case KindRateLimited:
		return "rate-limited"
	case KindBadGateway:
		return "bad-gateway"
	case KindNotImplemented:
		return "not-implemented"
	case KindUnavailable:
		return "unavailable"
	case KindPayloadTooLarge:
		return "payload-too-large"
	case KindRangeNotSatisfiable:
		return "range-not-satisfiable"
	default:
		return "internal"
	}
}

// FieldError descreve a falha de um campo específico (validação).
type FieldError struct {
	Field string `json:"field"`
	Code  string `json:"code"`
}

// Error é um erro de aplicação com contexto suficiente para virar uma resposta HTTP.
type Error struct {
	Kind   Kind
	Title  string
	Detail string
	// Code é um identificador estável para o cliente reagir (ex.: not_invited). Opcional.
	Code    string
	Fields  []FieldError
	wrapped error
}

func (e *Error) Error() string {
	if e.Detail != "" {
		return e.Title + ": " + e.Detail
	}
	return e.Title
}

func (e *Error) Unwrap() error { return e.wrapped }

// New cria um erro de aplicação.
func New(kind Kind, title, detail string) *Error {
	return &Error{Kind: kind, Title: title, Detail: detail}
}

// WithCode define o código estável do erro (campo `code` do problem+json).
func (e *Error) WithCode(code string) *Error {
	e.Code = code
	return e
}

// Wrap embrulha um erro existente preservando a cadeia para logs/observabilidade.
func Wrap(kind Kind, title string, err error) *Error {
	detail := ""
	if err != nil {
		detail = err.Error()
	}
	return &Error{Kind: kind, Title: title, Detail: detail, wrapped: err}
}

// Validation cria um erro de validação (422) com os campos que falharam.
func Validation(detail string, fields ...FieldError) *Error {
	return &Error{Kind: KindValidation, Title: "Validation failed", Detail: detail, Fields: fields}
}

// AsError extrai o *Error da cadeia (false se não houver um).
func AsError(err error) (*Error, bool) {
	var appErr *Error
	if errors.As(err, &appErr) {
		return appErr, true
	}
	return nil, false
}

// Problem é a representação RFC 7807 serializada na resposta.
type Problem struct {
	Type     string       `json:"type"`
	Title    string       `json:"title"`
	Status   int          `json:"status"`
	Detail   string       `json:"detail,omitempty"`
	Instance string       `json:"instance,omitempty"`
	TraceID  string       `json:"trace_id,omitempty"`
	Code     string       `json:"code,omitempty"`
	Errors   []FieldError `json:"errors,omitempty"`
}

// Write serializa o erro como application/problem+json. Qualquer erro não-*Error vira um 500
// genérico, sem vazar detalhes internos ao cliente.
func Write(w http.ResponseWriter, r *http.Request, err error) {
	var appErr *Error
	if !errors.As(err, &appErr) {
		appErr = Wrap(KindInternal, "Internal error", err)
	}
	status := appErr.Kind.status()

	detail := appErr.Detail
	if appErr.Kind == KindInternal {
		detail = "" // nunca expor detalhe interno
	}

	p := Problem{
		Type:     typeBase + appErr.Kind.slug(),
		Title:    appErr.Title,
		Status:   status,
		Detail:   detail,
		Instance: r.URL.Path,
		TraceID:  traceID(r),
		Code:     appErr.Code,
		Errors:   appErr.Fields,
	}

	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(p)
}

// traceID extrai o trace id do span ativo (vazio se não houver trace).
func traceID(r *http.Request) string {
	sc := trace.SpanContextFromContext(r.Context())
	if sc.HasTraceID() {
		return sc.TraceID().String()
	}
	return ""
}
