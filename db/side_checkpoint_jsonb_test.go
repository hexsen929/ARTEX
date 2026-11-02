package db

import (
	"bytes"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/sidequestion"
	"github.com/Autumn-27/norma/llm"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestSideSnapshotRawUnicodeJSONBRoundTrip(t *testing.T) {
	d, snapshot := sideFixture(t)
	const exactRunID int64 = 1793557371911967000 // greater than float64's exact integer range
	rawInput := json.RawMessage(`{"bad_low":"\udc00","bad_high":"\uD800","pair":"\uD83d\uDe00","literal":"\\udc00","nul":"before\u0000after","integer":1793557371911967000,"decimal":0.123456789012345678901234567890,"exponent":1.2300e+40}`)
	snapshot.RunID, snapshot.Version = exactRunID, 2
	snapshot.Request.Messages = []llm.Message{{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "unicode-fixture", Name: "fixture_only", Input: rawInput}}}}
	// Capture's normal clone path retains Input as RawMessage; normalizing only
	// Go strings would therefore leave the actual PostgreSQL failure reachable.
	cloned, err := sidequestion.CloneRequest(snapshot.Request)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Request = cloned
	if !bytes.Contains(cloned.Messages[0].Content[0].Input, []byte(`\udc00`)) {
		t.Fatal("fixture no longer reaches the RawMessage Unicode boundary")
	}
	original, err := json.Marshal(snapshot)
	if err != nil || !json.Valid(original) {
		t.Fatalf("Go marshaling should accept the fixture: %v", err)
	}
	var ignored []byte
	err = d.QueryRowContext(t.Context(), `SELECT $1::jsonb`, string(original)).Scan(&ignored)
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != "22P02" {
		t.Fatalf("raw isolated surrogate should reproduce the JSONB rejection: %v", err)
	}
	if err := d.SaveSideSnapshot(t.Context(), snapshot); err != nil {
		t.Fatalf("normalized checkpoint persistence failed: %v", err)
	}
	got, err := d.SideSnapshot(t.Context(), snapshot.Parent.Key())
	if err != nil || got == nil {
		t.Fatalf("checkpoint reload failed: %v", err)
	}
	if got.RunID != exactRunID || got.Version != 2 || got.Parent != snapshot.Parent {
		t.Fatalf("checkpoint identifiers or version lost precision: %+v", got)
	}
	if len(got.Request.Messages) != 1 || len(got.Request.Messages[0].Content) != 1 {
		t.Fatal("checkpoint message shape changed")
	}
	decoder := json.NewDecoder(bytes.NewReader(got.Request.Messages[0].Content[0].Input))
	decoder.UseNumber()
	var values map[string]any
	if err := decoder.Decode(&values); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"bad_low": "�", "bad_high": "�", "pair": "😀", "literal": `\udc00`, "nul": "beforeafter"} {
		if values[key] != want {
			t.Fatalf("field %s changed unexpectedly: got %v want %q", key, values[key], want)
		}
	}
	integer, ok := values["integer"].(json.Number)
	if !ok || integer.String() != "1793557371911967000" {
		t.Fatalf("RawMessage integer lost precision: %v", values["integer"])
	}
	for key, want := range map[string]string{"decimal": "0.123456789012345678901234567890", "exponent": "1.2300e+40"} {
		number, ok := values[key].(json.Number)
		if !ok {
			t.Fatalf("field %s is no longer numeric: %v", key, values[key])
		}
		actual, actualOK := new(big.Rat).SetString(number.String())
		expected, expectedOK := new(big.Rat).SetString(want)
		if !actualOK || !expectedOK || actual.Cmp(expected) != 0 {
			t.Fatalf("field %s lost exact numeric value: got %s want %s", key, number, want)
		}
	}
	if strings.Contains(string(rawInput), `\ufffd`) || !bytes.Contains(rawInput, []byte(`\udc00`)) {
		t.Fatal("checkpoint persistence rewrote original tool input")
	}
}

func TestJSONBNormalizationAcceptsUnicodeBoundaries(t *testing.T) {
	d, _ := sideFixture(t)
	for _, raw := range []string{
		`{"value":"\udc00"}`,
		`{"value":"\uD800"}`,
		`{"value":"\ud800\u0000\udc00"}`,
		`{"value":"\ud800\ud800\udc00"}`,
		`{"\uDC00":"\uD800"}`,
		`{"value":"\\\uDC00"}`,
		`{"value":"\\udc00","pair":"\uD83d\uDe00","number":1793557371911967000}`,
	} {
		var stored []byte
		if err := d.QueryRowContext(t.Context(), `SELECT $1::jsonb`, string(jsonbClean([]byte(raw)))).Scan(&stored); err != nil {
			t.Fatalf("normalized boundary rejected: %s: %v", raw, err)
		}
		if !json.Valid(stored) {
			t.Fatal("database returned invalid normalized JSON")
		}
	}
}
