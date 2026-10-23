package ai

import (
	"fmt"
	"strings"
)

// systemPrompt vale para todas as execuções (--append-system-prompt). O RbNotes é um app PESSOAL de
// notas de voz: a transcrição é feita no Mac e só o texto chega aqui. Sem acesso à web nem a
// ferramentas (AllowedTools vazio): a IA trabalha SÓ com o que recebe no prompt.
const systemPrompt = `Você é o assistente do RbNotes, um app pessoal de notas de voz de um usuário brasileiro.

Regras gerais (obrigatório):
- Responda SEMPRE em português do Brasil correto e natural, com boa concordância.
- Trabalhe apenas com o conteúdo fornecido no prompt. Você NÃO tem acesso à internet nem a ferramentas. Nunca invente fatos, nomes, números, datas ou trechos.
- Seja direto e útil: frases curtas, voz ativa.
- Privacidade: não repita dados sensíveis desnecessariamente; não faça suposições sobre a vida do usuário além do que o texto disser.
- Quando não souber, diga que não há informação suficiente — não preencha com invenção.
- Entregue SEMPRE a saída no formato estruturado pedido (o schema).`

// styleHint traduz o estilo pedido para instruções de escrita.
func styleHint(style string) string {
	switch style {
	case "detalhado":
		return "Estilo detalhado: explique com contexto, mas sem encher linguiça."
	case "topicos":
		return "Estilo em tópicos: prefira listas curtas e objetivas."
	default:
		return "Estilo objetivo: vá direto ao ponto, só o essencial."
	}
}

// lengthHint traduz o tamanho pedido.
func lengthHint(length string) string {
	switch length {
	case "longo":
		return "Tamanho longo: pode desenvolver mais os pontos e o resumo."
	case "medio":
		return "Tamanho médio: equilibre concisão e completude."
	default:
		return "Tamanho curto: o mínimo necessário para registrar a nota."
	}
}

// transcriptBlock monta a transcrição numerada (índice, falante, tempo e texto) para o prompt.
func transcriptBlock(segs []Segment) string {
	var b strings.Builder
	for _, s := range segs {
		fmt.Fprintf(&b, "[%d] (%s, %dms–%dms) %s\n", s.Index, speakerLabel(s.Speaker), s.StartMs, s.EndMs, strings.TrimSpace(s.Text))
	}
	return b.String()
}

func speakerLabel(s string) string {
	switch s {
	case "me":
		return "Você"
	case "others", "":
		return "Outro"
	default:
		return s
	}
}

// enrichPrompt monta o pedido de enriquecimento da nota.
func enrichPrompt(in EnrichInput) string {
	var b strings.Builder
	b.WriteString("Enriqueça a nota de voz abaixo a partir da transcrição (os números entre colchetes são os índices dos segmentos).\n\n")
	fmt.Fprintf(&b, "%s %s\n", styleHint(in.Style), lengthHint(in.Length))
	if in.Folder != "" {
		fmt.Fprintf(&b, "Pasta da nota: %s\n", in.Folder)
	}
	if len(in.Speakers) > 0 {
		fmt.Fprintf(&b, "Falantes conhecidos informados: %s\n", strings.Join(in.Speakers, ", "))
	}
	if in.ExtractTasks {
		b.WriteString("Extraia tarefas (actions) com responsável e prazo quando houver; use null em due se não houver data.\n")
	} else {
		b.WriteString("NÃO extraia tarefas: devolva actions como lista vazia.\n")
	}
	b.WriteString("Identifique os nomes dos OUTROS falantes (além de \"me\" = Você) pelo contexto da conversa; em speakerNames.segments marque inferred=true quando o nome foi deduzido (não dito explicitamente).\n")
	b.WriteString("A citação (quote) deve usar o índice e o startMs de um segmento REAL; use null se nenhum trecho se destacar.\n\n")
	b.WriteString("Transcrição:\n")
	b.WriteString(transcriptBlock(in.Transcript))
	return b.String()
}

// chatTurn é uma mensagem anterior da conversa (reconstrói o contexto sem --resume).
type chatTurn struct {
	Role    string
	Content string
}

const maxTranscriptTurns = 20

// chatPrompt monta o turno do chat. Com sessão (--resume), só a pergunta; sem sessão, o histórico
// recente e a transcrição (se fornecida) entram no prompt.
func chatPrompt(message string, history []chatTurn, transcript []Segment, resumed bool) string {
	if resumed {
		return message
	}
	var b strings.Builder
	b.WriteString("Converse sobre a nota de voz abaixo. Responda de forma curta e prática (markdown simples).\n")
	b.WriteString("Só cite trechos (cites.atMs) que existam de fato na transcrição; se não houver transcrição, não cite nada.\n\n")
	if len(transcript) > 0 {
		b.WriteString("Transcrição:\n")
		b.WriteString(transcriptBlock(transcript))
		b.WriteString("\n")
	}
	if len(history) > maxTranscriptTurns {
		history = history[len(history)-maxTranscriptTurns:]
	}
	if len(history) > 0 {
		b.WriteString("Conversa até agora:\n")
		for _, m := range history {
			who := "Eu"
			if m.Role == "assistant" {
				who = "Você"
			}
			fmt.Fprintf(&b, "%s: %s\n", who, m.Content)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "Minha pergunta: %s", message)
	return b.String()
}

// askCandidate é uma nota candidata (resumida) para o `ask`.
type askCandidate struct {
	NoteID string
	Title  string
	Tldr   string
}

// askPrompt monta o pedido do `ask` com as notas candidatas (já selecionadas pelo servidor).
func askPrompt(question string, candidates []askCandidate) string {
	var b strings.Builder
	b.WriteString("Responda à pergunta usando SOMENTE as notas abaixo. Não invente nada que não esteja nelas.\n")
	b.WriteString("Em sources, cite apenas as notas (noteId) que você realmente usou, dentre as fornecidas.\n")
	b.WriteString("Se as notas não responderem, diga isso claramente.\n\n")
	b.WriteString("Notas candidatas:\n")
	for _, c := range candidates {
		fmt.Fprintf(&b, "- noteId: %s\n  título: %s\n", c.NoteID, c.Title)
		if strings.TrimSpace(c.Tldr) != "" {
			fmt.Fprintf(&b, "  resumo: %s\n", c.Tldr)
		}
	}
	fmt.Fprintf(&b, "\nPergunta: %s", question)
	return b.String()
}

// collectionRulePrompt monta o pedido de regra de coleção.
func collectionRulePrompt(description string) string {
	return "Transforme a descrição abaixo numa regra de coleção de notas.\n" +
		"Gere um label curto, o ruleText (a regra em linguagem natural) e o objeto rule com keywords (sempre) " +
		"e, quando fizer sentido, folders, minSpeakers (inteiro >= 1) e openActions (boolean). O cliente avalia a regra localmente.\n\n" +
		"Descrição: " + description
}

// diaryMonthPrompt monta o resumo mensal a partir das notas do mês.
func diaryMonthPrompt(month string, notes []askCandidate) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Faça um resumo do mês %s a partir das notas abaixo e liste os temas recorrentes com uma contagem aproximada.\n", month)
	b.WriteString("Baseie-se só nas notas fornecidas.\n\n")
	if len(notes) == 0 {
		b.WriteString("(Não há notas neste mês — devolva um resumo curto dizendo isso e temas vazios.)\n")
		return b.String()
	}
	b.WriteString("Notas do mês:\n")
	for _, n := range notes {
		fmt.Fprintf(&b, "- %s", n.Title)
		if strings.TrimSpace(n.Tldr) != "" {
			fmt.Fprintf(&b, ": %s", n.Tldr)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// dailyPromptPrompt monta o disparador de escrita do dia (opcionalmente informado por notas recentes).
func dailyPromptPrompt(date string, recent []askCandidate) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Crie UM disparador de escrita reflexiva para o dia %s, curto e pessoal, em pt-BR.\n", date)
	if len(recent) > 0 {
		b.WriteString("Pode se inspirar (sem copiar) nos temas recentes das notas abaixo:\n")
		for _, n := range recent {
			fmt.Fprintf(&b, "- %s\n", n.Title)
		}
	}
	return b.String()
}
