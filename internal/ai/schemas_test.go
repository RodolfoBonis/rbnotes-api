package ai

import (
	"strings"
	"context"
	"github.com/jackc/pgx/v5/pgtype"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateNoteAi(t *testing.T) {
	good := `{
      "title":"Reunião de produto","tldr":"Alinhamos o roadmap do trimestre.",
      "points":["Priorizar busca","Adiar export"],
      "quote":{"segmentIndex":1,"text":"vamos focar na busca","speaker":"Camila","atMs":188000},
      "actions":[{"text":"Especificar busca","owner":"Você","due":"2026-10-30"}],
      "decisions":["Busca primeiro"],"questions":["Qual prazo do export?"],
      "speakerNames":{"others":["Camila","Rafael"],"segments":[{"index":1,"speaker":"Camila","inferred":true}]}
    }`
	out, err := ValidateNoteAi([]byte(good), 3)
	require.NoError(t, err)
	var n NoteAi
	require.NoError(t, json.Unmarshal(out, &n))
	require.Equal(t, "Reunião de produto", n.Title)
	require.Len(t, n.Actions, 1)
	require.True(t, n.SpeakerNames.Segments[0].Inferred)

	// title/tldr obrigatórios.
	_, err = ValidateNoteAi([]byte(`{"title":"","tldr":"x","points":[],"quote":null,"actions":[],"decisions":[],"questions":[],"speakerNames":{"others":[],"segments":[]}}`), 3)
	require.ErrorIs(t, err, ErrInvalidOutput)

	// quote.segmentIndex fora da faixa (nSegments=2 → índices 0..1).
	bad := `{"title":"a","tldr":"b","points":[],"quote":{"segmentIndex":5,"text":"x","speaker":"y","atMs":0},"actions":[],"decisions":[],"questions":[],"speakerNames":{"others":[],"segments":[]}}`
	_, err = ValidateNoteAi([]byte(bad), 2)
	require.ErrorIs(t, err, ErrInvalidOutput)

	// due inválido.
	badDue := `{"title":"a","tldr":"b","points":[],"quote":null,"actions":[{"text":"t","owner":"Você","due":"30/10/2026"}],"decisions":[],"questions":[],"speakerNames":{"others":[],"segments":[]}}`
	_, err = ValidateNoteAi([]byte(badDue), 0)
	require.ErrorIs(t, err, ErrInvalidOutput)

	// campo desconhecido é rejeitado (DisallowUnknownFields).
	extra := `{"title":"a","tldr":"b","points":[],"quote":null,"actions":[],"decisions":[],"questions":[],"speakerNames":{"others":[],"segments":[]},"hack":1}`
	_, err = ValidateNoteAi([]byte(extra), 0)
	require.ErrorIs(t, err, ErrInvalidOutput)
}

func TestValidateChatFiltersCites(t *testing.T) {
	raw := `{"text":"Foquem na busca.","cites":[{"atMs":188000},{"atMs":999999}]}`
	// Só 188000 é um startMs real.
	out, err := ValidateChat([]byte(raw), map[int64]bool{188000: true})
	require.NoError(t, err)
	var a ChatAnswer
	require.NoError(t, json.Unmarshal(out, &a))
	require.Equal(t, []Cite{{AtMs: 188000}}, a.Cites)

	// Sem conjunto permitido, mantém as citações como vieram.
	out, err = ValidateChat([]byte(raw), nil)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(out, &a))
	require.Len(t, a.Cites, 2)

	_, err = ValidateChat([]byte(`{"text":"  ","cites":[]}`), nil)
	require.ErrorIs(t, err, ErrInvalidOutput)
}

func TestValidateAskRestrictsSources(t *testing.T) {
	raw := `{"text":"Segundo a nota A.","sources":[{"noteId":"a","title":"mentira"},{"noteId":"z","title":"inventada"},{"noteId":"a","title":"dup"}]}`
	cands := map[string]string{"a": "Nota A", "b": "Nota B"}
	out, err := ValidateAsk([]byte(raw), cands)
	require.NoError(t, err)
	var a AskAnswer
	require.NoError(t, json.Unmarshal(out, &a))
	// Só "a" (candidata), sem duplicar, e com o título canônico do servidor.
	require.Equal(t, []Source{{NoteID: "a", Title: "Nota A"}}, a.Sources)
}

func TestValidateCollectionRule(t *testing.T) {
	raw := `{"label":"Reuniões","ruleText":"Notas com várias pessoas","rule":{"keywords":["reunião","call"],"minSpeakers":2,"openActions":true}}`
	out, err := ValidateCollectionRule([]byte(raw))
	require.NoError(t, err)
	var c CollectionRuleResult
	require.NoError(t, json.Unmarshal(out, &c))
	require.Equal(t, 2, *c.Rule.MinSpeakers)

	_, err = ValidateCollectionRule([]byte(`{"label":"","ruleText":"x","rule":{"keywords":[]}}`))
	require.ErrorIs(t, err, ErrInvalidOutput)
}

func TestValidateDiaryAndDaily(t *testing.T) {
	_, err := ValidateDiaryMonth([]byte(`{"summary":"Mês produtivo","themes":[{"name":"trabalho","count":4}]}`))
	require.NoError(t, err)
	_, err = ValidateDiaryMonth([]byte(`{"summary":"","themes":[]}`))
	require.ErrorIs(t, err, ErrInvalidOutput)

	_, err = ValidateDailyPrompt([]byte(`{"prompt":"O que te deu energia hoje?"}`))
	require.NoError(t, err)
	_, err = ValidateDailyPrompt([]byte(`{"prompt":""}`))
	require.ErrorIs(t, err, ErrInvalidOutput)
}

func TestStrictDecodeRejectsTrailingData(t *testing.T) {
	// Lixo depois do JSON é rejeitado (um segundo objeto).
	_, err := ValidateChat([]byte(`{"text":"oi","cites":[]} {"x":1}`), nil)
	require.ErrorIs(t, err, ErrInvalidOutput)
}

func TestValidateNoteAiCapsStrings(t *testing.T) {
	longTitle := strings.Repeat("a", maxTitleLen+50)
	raw := []byte(`{"title":"` + longTitle + `","tldr":"ok","points":[],"quote":null,"actions":[],"decisions":[],"questions":[],"speakerNames":{"others":[],"segments":[]}}`)
	out, err := ValidateNoteAi(raw, 0)
	require.NoError(t, err)
	var n NoteAi
	require.NoError(t, json.Unmarshal(out, &n))
	require.Len(t, []rune(n.Title), maxTitleLen)
}

func TestValidateTranscriptLimits(t *testing.T) {
	segs := make([]Segment, maxTranscriptSegments+1)
	require.ErrorIs(t, validateTranscript(segs), ErrBadInput)

	big := Segment{Text: strings.Repeat("x", maxTranscriptChars+1)}
	require.ErrorIs(t, validateTranscript([]Segment{big}), ErrBadInput)

	require.NoError(t, validateTranscript([]Segment{{Text: "curto"}}))
}

func TestRequestChatRejectsBadThreadID(t *testing.T) {
	svc := New(Deps{})
	_, err := svc.RequestChat(context.Background(), pgtype.UUID{}, ChatInput{Message: "oi", ThreadID: "bad id/with*chars"})
	require.ErrorIs(t, err, ErrBadInput)
}
