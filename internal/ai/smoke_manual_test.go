//go:build manual

package ai

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/RodolfoBonis/rbnotes-api/internal/ai/claude"
)

// Smoke manual com a CLI real (precisa de `claude` logado e CLAUDE_CODE_OAUTH_TOKEN no ambiente):
//
//	CLAUDE_CODE_OAUTH_TOKEN=$(cat token) go test -tags manual -run TestRealEnrich ./internal/ai/
func TestRealEnrich(t *testing.T) {
	if os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") == "" {
		t.Skip("CLAUDE_CODE_OAUTH_TOKEN ausente")
	}
	r := claude.New("claude", os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"), t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	in := EnrichInput{
		Style: "objetivo", Length: "curto", ExtractTasks: true,
		Speakers: []string{"Camila"},
		Transcript: []Segment{
			{Index: 0, Speaker: "me", StartMs: 0, EndMs: 5000, Text: "Oi Camila, bom te ver. Vamos falar do roadmap?"},
			{Index: 1, Speaker: "others", StartMs: 5000, EndMs: 12000, Text: "Claro! Acho que a gente devia priorizar a busca antes do export."},
			{Index: 2, Speaker: "me", StartMs: 12000, EndMs: 16000, Text: "Fechado. Eu especifico a busca até dia 30 de outubro."},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	n := map[claude.EventType]int{}
	res, err := r.Run(ctx, claude.Request{
		Prompt: enrichPrompt(in), SystemPrompt: systemPrompt, Model: os.Getenv("AI_MODEL"),
		JSONSchema: NoteEnrichSchema, AllowedTools: AllowedTools, Workdir: t.TempDir(),
	}, func(e claude.Event) { n[e.Type]++ })
	if err != nil || res.Err != "" {
		t.Fatalf("err=%v res.Err=%s", err, res.Err)
	}
	out, verr := ValidateNoteAi(res.Structured, len(in.Transcript))
	if verr != nil {
		t.Fatalf("validação: %v\n%s", verr, res.Structured)
	}
	t.Logf("events=%v cost=%.3f\n%s", n, res.CostUSD, out)
}
