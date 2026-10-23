// Package ai gera o enriquecimento de notas, o chat, o `ask` e os auxiliares de coleção/diário
// usando a CLI do Claude Code (ver internal/ai/claude). Toda saída é estruturada: o JSON Schema vai
// para a CLI (--json-schema) e o resultado é revalidado aqui, de forma estrita, antes de persistir.
package ai

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SchemaVersion é a versão dos schemas de saída da IA.
const SchemaVersion = 1

// ErrInvalidOutput indica saída que não segue o schema esperado.
var ErrInvalidOutput = errors.New("saída da IA fora do schema")

// Segment é um trecho da transcrição (contrato do desktop).
type Segment struct {
	Index   int    `json:"index"`
	Speaker string `json:"speaker"`
	StartMs int64  `json:"startMs"`
	EndMs   int64  `json:"endMs"`
	Text    string `json:"text"`
}

/* ── note_enrich: NoteAi ──────────────────────────────────────────────────────────────── */

// Quote é a citação em destaque da nota.
type Quote struct {
	SegmentIndex int    `json:"segmentIndex"`
	Text         string `json:"text"`
	Speaker      string `json:"speaker"`
	AtMs         int64  `json:"atMs"`
}

// ActionItem é uma tarefa extraída da nota.
type ActionItem struct {
	Text  string  `json:"text"`
	Owner string  `json:"owner"`
	Due   *string `json:"due"` // "YYYY-MM-DD" ou null
}

// SpeakerSeg marca o falante inferido de um segmento.
type SpeakerSeg struct {
	Index    int    `json:"index"`
	Speaker  string `json:"speaker"`
	Inferred bool   `json:"inferred"`
}

// SpeakerNames agrupa os falantes identificados além de "me".
type SpeakerNames struct {
	Others   []string     `json:"others"`
	Segments []SpeakerSeg `json:"segments"`
}

// NoteAi é o enriquecimento de uma nota (schema note_enrich).
type NoteAi struct {
	Title        string       `json:"title"`
	Tldr         string       `json:"tldr"`
	Points       []string     `json:"points"`
	Quote        *Quote       `json:"quote"`
	Actions      []ActionItem `json:"actions"`
	Decisions    []string     `json:"decisions"`
	Questions    []string     `json:"questions"`
	SpeakerNames SpeakerNames `json:"speakerNames"`
}

const strList = `{"type":"array","items":{"type":"string"}}`

// NoteEnrichSchema é o JSON Schema passado à CLI para o enriquecimento da nota.
var NoteEnrichSchema = compact(`{
  "type":"object","additionalProperties":false,
  "required":["title","tldr","points","quote","actions","decisions","questions","speakerNames"],
  "properties":{
    "title":{"type":"string","description":"Título curto e específico da nota, em pt-BR."},
    "tldr":{"type":"string","description":"Resumo em 1–3 frases, pt-BR, direto."},
    "points":{"type":"array","items":{"type":"string"},"description":"Pontos principais, curtos e objetivos."},
    "quote":{"type":["object","null"],"additionalProperties":false,
      "required":["segmentIndex","text","speaker","atMs"],
      "properties":{
        "segmentIndex":{"type":"integer","minimum":0,"description":"Índice do segmento citado."},
        "text":{"type":"string","description":"Trecho literal da transcrição."},
        "speaker":{"type":"string","description":"Quem falou (nome ou \"Você\")."},
        "atMs":{"type":"integer","minimum":0,"description":"Início do trecho em ms (startMs do segmento)."}
      },"description":"Citação marcante da nota, ou null se não houver."},
    "actions":{"type":"array","items":{"type":"object","additionalProperties":false,
      "required":["text","owner","due"],
      "properties":{
        "text":{"type":"string"},
        "owner":{"type":"string","description":"Responsável (\"Você\" ou nome do falante)."},
        "due":{"type":["string","null"],"description":"Prazo YYYY-MM-DD ou null."}
      }},"description":"Tarefas extraídas (vazio se extractTasks=false ou não houver)."},
    "decisions":` + strList + `,
    "questions":` + strList + `,
    "speakerNames":{"type":"object","additionalProperties":false,
      "required":["others","segments"],
      "properties":{
        "others":` + strList + `,
        "segments":{"type":"array","items":{"type":"object","additionalProperties":false,
          "required":["index","speaker","inferred"],
          "properties":{
            "index":{"type":"integer","minimum":0},
            "speaker":{"type":"string"},
            "inferred":{"type":"boolean","description":"true quando o nome foi deduzido do contexto."}
          }}}
      },"description":"Nomes dos outros falantes (além de \"me\"), inferidos do contexto."}
  }}`)

// ValidateNoteAi valida e normaliza o JSON do enrich contra o schema e a transcrição (nSegments =
// número de segmentos da nota; índices fora da faixa são rejeitados).
func ValidateNoteAi(raw []byte, nSegments int) ([]byte, error) {
	var n NoteAi
	if err := strictDecode(raw, &n); err != nil {
		return nil, err
	}
	if strings.TrimSpace(n.Title) == "" || strings.TrimSpace(n.Tldr) == "" {
		return nil, fmt.Errorf("%w: title e tldr são obrigatórios", ErrInvalidOutput)
	}
	if n.Quote != nil {
		if err := checkIndex(n.Quote.SegmentIndex, nSegments, "quote.segmentIndex"); err != nil {
			return nil, err
		}
		if n.Quote.AtMs < 0 {
			return nil, fmt.Errorf("%w: quote.atMs negativo", ErrInvalidOutput)
		}
	}
	for i := range n.Actions {
		if strings.TrimSpace(n.Actions[i].Text) == "" {
			return nil, fmt.Errorf("%w: ação sem texto", ErrInvalidOutput)
		}
		if err := checkDue(n.Actions[i].Due); err != nil {
			return nil, err
		}
	}
	for _, s := range n.SpeakerNames.Segments {
		if err := checkIndex(s.Index, nSegments, "speakerNames.segments[].index"); err != nil {
			return nil, err
		}
	}
	n.Title = capRunes(strings.TrimSpace(n.Title), maxTitleLen)
	n.Tldr = capRunes(strings.TrimSpace(n.Tldr), maxTldrLen)
	n.Points, n.Decisions, n.Questions = clean(n.Points), clean(n.Decisions), clean(n.Questions)
	n.SpeakerNames.Others = clean(n.SpeakerNames.Others)
	return json.Marshal(n)
}

/* ── note_chat ────────────────────────────────────────────────────────────────────────── */

// Cite é uma citação por tempo (ms) numa resposta de chat.
type Cite struct {
	AtMs int64 `json:"atMs"`
}

// ChatAnswer é a resposta do chat (schema note_chat).
type ChatAnswer struct {
	Text  string `json:"text"`
	Cites []Cite `json:"cites"`
}

// NoteChatSchema é o JSON Schema do chat.
var NoteChatSchema = compact(`{
  "type":"object","additionalProperties":false,
  "required":["text","cites"],
  "properties":{
    "text":{"type":"string","description":"Resposta em pt-BR, markdown simples."},
    "cites":{"type":"array","items":{"type":"object","additionalProperties":false,
      "required":["atMs"],
      "properties":{"atMs":{"type":"integer","minimum":0,"description":"Início em ms de um trecho REAL citado."}}},
      "description":"Citações por tempo; apenas de trechos que existem na transcrição."}
  }}`)

// ValidateChat valida a resposta do chat. Quando allowed não é vazio, só mantém citações cujo atMs
// pertence a um startMs real de segmento (ancoragem das citações); vazio = não filtra.
func ValidateChat(raw []byte, allowed map[int64]bool) ([]byte, error) {
	var a ChatAnswer
	if err := strictDecode(raw, &a); err != nil {
		return nil, err
	}
	if strings.TrimSpace(a.Text) == "" {
		return nil, fmt.Errorf("%w: resposta vazia", ErrInvalidOutput)
	}
	a.Text = capRunes(a.Text, maxTextLen)
	if len(allowed) > 0 {
		kept := make([]Cite, 0, len(a.Cites))
		for _, c := range a.Cites {
			if allowed[c.AtMs] {
				kept = append(kept, c)
			}
		}
		a.Cites = kept
	}
	if a.Cites == nil {
		a.Cites = []Cite{}
	}
	return json.Marshal(a)
}

/* ── ask ──────────────────────────────────────────────────────────────────────────────── */

// Source é uma nota citada como fonte do `ask`.
type Source struct {
	NoteID string `json:"noteId"`
	Title  string `json:"title"`
}

// AskAnswer é a resposta do `ask` (schema ask).
type AskAnswer struct {
	Text    string   `json:"text"`
	Sources []Source `json:"sources"`
}

// AskSchema é o JSON Schema do `ask`.
var AskSchema = compact(`{
  "type":"object","additionalProperties":false,
  "required":["text","sources"],
  "properties":{
    "text":{"type":"string","description":"Resposta em pt-BR com base SÓ nas notas fornecidas."},
    "sources":{"type":"array","items":{"type":"object","additionalProperties":false,
      "required":["noteId","title"],
      "properties":{"noteId":{"type":"string"},"title":{"type":"string"}}},
      "description":"Notas citadas; apenas dentre as candidatas fornecidas."}
  }}`)

// ValidateAsk valida a resposta do `ask`. Só mantém fontes cujo noteId está entre as candidatas
// (candidates mapeia noteId -> título canônico, que sobrescreve o título vindo do modelo).
func ValidateAsk(raw []byte, candidates map[string]string) ([]byte, error) {
	var a AskAnswer
	if err := strictDecode(raw, &a); err != nil {
		return nil, err
	}
	if strings.TrimSpace(a.Text) == "" {
		return nil, fmt.Errorf("%w: resposta vazia", ErrInvalidOutput)
	}
	a.Text = capRunes(a.Text, maxTextLen)
	kept := make([]Source, 0, len(a.Sources))
	seen := map[string]bool{}
	for _, src := range a.Sources {
		title, ok := candidates[src.NoteID]
		if !ok || seen[src.NoteID] {
			continue
		}
		seen[src.NoteID] = true
		kept = append(kept, Source{NoteID: src.NoteID, Title: title})
	}
	a.Sources = kept
	return json.Marshal(a)
}

/* ── collection_rule ──────────────────────────────────────────────────────────────────── */

// CollectionRule é a regra derivada de uma descrição em linguagem natural.
type CollectionRule struct {
	Keywords    []string `json:"keywords"`
	Folders     []string `json:"folders,omitempty"`
	MinSpeakers *int     `json:"minSpeakers,omitempty"`
	OpenActions *bool    `json:"openActions,omitempty"`
}

// CollectionRuleResult é a resposta de collection_rule.
type CollectionRuleResult struct {
	Label    string         `json:"label"`
	RuleText string         `json:"ruleText"`
	Rule     CollectionRule `json:"rule"`
}

// CollectionRuleSchema é o JSON Schema de collection_rule.
var CollectionRuleSchema = compact(`{
  "type":"object","additionalProperties":false,
  "required":["label","ruleText","rule"],
  "properties":{
    "label":{"type":"string","description":"Nome curto da coleção."},
    "ruleText":{"type":"string","description":"A regra em linguagem natural (pt-BR)."},
    "rule":{"type":"object","additionalProperties":false,
      "required":["keywords"],
      "properties":{
        "keywords":` + strList + `,
        "folders":` + strList + `,
        "minSpeakers":{"type":"integer","minimum":1},
        "openActions":{"type":"boolean"}
      }}
  }}`)

// ValidateCollectionRule valida a regra de coleção.
func ValidateCollectionRule(raw []byte) ([]byte, error) {
	var c CollectionRuleResult
	if err := strictDecode(raw, &c); err != nil {
		return nil, err
	}
	if strings.TrimSpace(c.Label) == "" {
		return nil, fmt.Errorf("%w: label obrigatório", ErrInvalidOutput)
	}
	c.Rule.Keywords = clean(c.Rule.Keywords)
	c.Rule.Folders = clean(c.Rule.Folders)
	if c.Rule.MinSpeakers != nil && *c.Rule.MinSpeakers < 1 {
		return nil, fmt.Errorf("%w: minSpeakers deve ser >= 1", ErrInvalidOutput)
	}
	return json.Marshal(c)
}

/* ── diary_month ──────────────────────────────────────────────────────────────────────── */

// Theme é um tema recorrente do mês.
type Theme struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// DiaryMonth é a resposta de diary_month.
type DiaryMonth struct {
	Summary string  `json:"summary"`
	Themes  []Theme `json:"themes"`
}

// DiaryMonthSchema é o JSON Schema de diary_month.
var DiaryMonthSchema = compact(`{
  "type":"object","additionalProperties":false,
  "required":["summary","themes"],
  "properties":{
    "summary":{"type":"string","description":"Resumo do mês em pt-BR."},
    "themes":{"type":"array","items":{"type":"object","additionalProperties":false,
      "required":["name","count"],
      "properties":{"name":{"type":"string"},"count":{"type":"integer","minimum":0}}}}
  }}`)

// ValidateDiaryMonth valida o resumo mensal.
func ValidateDiaryMonth(raw []byte) ([]byte, error) {
	var d DiaryMonth
	if err := strictDecode(raw, &d); err != nil {
		return nil, err
	}
	if strings.TrimSpace(d.Summary) == "" {
		return nil, fmt.Errorf("%w: summary obrigatório", ErrInvalidOutput)
	}
	return json.Marshal(d)
}

/* ── daily_prompt ─────────────────────────────────────────────────────────────────────── */

// DailyPrompt é a resposta de daily_prompt.
type DailyPrompt struct {
	Prompt string `json:"prompt"`
}

// DailyPromptSchema é o JSON Schema de daily_prompt.
var DailyPromptSchema = compact(`{
  "type":"object","additionalProperties":false,
  "required":["prompt"],
  "properties":{"prompt":{"type":"string","description":"Um disparador de escrita para o dia, pt-BR."}}
}`)

// ValidateDailyPrompt valida o disparador do dia.
func ValidateDailyPrompt(raw []byte) ([]byte, error) {
	var d DailyPrompt
	if err := strictDecode(raw, &d); err != nil {
		return nil, err
	}
	if strings.TrimSpace(d.Prompt) == "" {
		return nil, fmt.Errorf("%w: prompt vazio", ErrInvalidOutput)
	}
	return json.Marshal(d)
}

/* ── helpers ──────────────────────────────────────────────────────────────────────────── */

// strictDecode decodifica rejeitando campos desconhecidos (DisallowUnknownFields). O "required" do
// schema é checado pela própria CLI; aqui garantimos o formato e capturamos campos extras.
func strictDecode(raw []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidOutput, err)
	}
	// Rejeita lixo depois do JSON (ex.: um segundo objeto, texto solto): só um valor é válido.
	if dec.More() {
		return fmt.Errorf("%w: dados após o JSON", ErrInvalidOutput)
	}
	return nil
}

// Limites de tamanho da saída da IA (defesa contra respostas patológicas).
const (
	maxTitleLen = 300
	maxTldrLen  = 4000
	maxTextLen  = 20000
)

// capRunes corta s em no máximo n runes (sem partir um code point).
func capRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func checkIndex(idx, n int, field string) error {
	if n <= 0 {
		return nil // sem transcrição conhecida: não dá para validar o índice
	}
	if idx < 0 || idx >= n {
		return fmt.Errorf("%w: %s fora da faixa (0..%d)", ErrInvalidOutput, field, n-1)
	}
	return nil
}

func checkDue(due *string) error {
	if due == nil || *due == "" {
		return nil
	}
	if _, err := time.Parse("2006-01-02", *due); err != nil {
		return fmt.Errorf("%w: due deve ser YYYY-MM-DD ou null", ErrInvalidOutput)
	}
	return nil
}

// clean remove itens vazios e garante [] (nunca null) no JSON.
func clean(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func compact(s string) string {
	var b bytes.Buffer
	if err := json.Compact(&b, []byte(s)); err != nil {
		panic("schema inválido: " + err.Error())
	}
	return b.String()
}
