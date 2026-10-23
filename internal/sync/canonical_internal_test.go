package sync

import "testing"

func TestCanonicalJSONIgnoresFormattingAndKeyOrder(t *testing.T) {
	a := canonicalJSON([]byte(`{"title":"B","n":12345678901234567890}`))
	b := canonicalJSON([]byte(`{ "n": 12345678901234567890, "title": "B" }`)) // como o JSONB do Postgres
	if string(a) != string(b) {
		t.Fatalf("canonical forms differ: %s vs %s", a, b)
	}
	if string(a) != `{"n":12345678901234567890,"title":"B"}` {
		t.Fatalf("unexpected canonical form (precision lost?): %s", a)
	}
	if got := canonicalJSON([]byte(`not json`)); string(got) != "not json" {
		t.Fatalf("invalid JSON must be returned as-is, got %s", got)
	}
}
