package mcpulse

// The cross-language contract.
//
// testdata/canonical.json is the shared conformance suite, copied from
// packages/schemas/fixtures in the mcpulse monorepo. Every other MCPulse SDK
// runs the same file. If it passes in all of them, their hashes are
// interchangeable and a customer running more than one sees one set of numbers
// rather than several.
//
// Never edit a fixture to make a failure go away — these hashes are in the
// product's history, and rewriting one rewrites what every stored row means.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"
)

type fixtureFile struct {
	Algorithm   string    `json:"algorithm"`
	WireVersion int       `json:"wire_version"`
	Fixtures    []fixture `json:"fixtures"`
}

type fixture struct {
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	Canonical string          `json:"canonical"`
	ArgsHash  string          `json:"args_hash"`
}

func loadFixtures(t *testing.T) fixtureFile {
	t.Helper()

	raw, err := os.ReadFile("testdata/canonical.json")
	if err != nil {
		t.Fatalf("reading fixtures: %v", err)
	}

	var file fixtureFile
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("parsing fixtures: %v", err)
	}
	return file
}

func TestFixtureFilePinsTheAlgorithm(t *testing.T) {
	file := loadFixtures(t)
	if file.Algorithm != "sha256/rfc8785/hex12" {
		t.Errorf("algorithm = %q", file.Algorithm)
	}
	if file.WireVersion != 1 {
		t.Errorf("wire_version = %d", file.WireVersion)
	}
}

func TestMatchesTheTypeScriptSDK(t *testing.T) {
	file := loadFixtures(t)
	if len(file.Fixtures) == 0 {
		t.Fatal("no fixtures loaded")
	}

	for _, f := range file.Fixtures {
		t.Run(f.Name, func(t *testing.T) {
			var input any
			if err := json.Unmarshal(f.Input, &input); err != nil {
				t.Fatalf("decoding input: %v", err)
			}

			got, err := Canonicalize(input)
			if err != nil {
				t.Fatalf("Canonicalize: %v", err)
			}
			if got != f.Canonical {
				t.Errorf("canonical form\n  want %s\n  got  %s", f.Canonical, got)
			}

			sum := sha256.Sum256([]byte(f.Canonical))
			if want := hex.EncodeToString(sum[:])[:12]; want != f.ArgsHash {
				t.Errorf("fixture hash does not match its own canonical form: %s vs %s", f.ArgsHash, want)
			}
			if got := ArgsHash(input); got != f.ArgsHash {
				t.Errorf("ArgsHash = %s, want %s", got, f.ArgsHash)
			}
		})
	}
}

func TestNumberFormatting(t *testing.T) {
	// ECMAScript Number::toString, which is where Go's own formatting differs.
	cases := []struct {
		value float64
		want  string
	}{
		{1.0, "1"},
		{-0.0, "0"},
		{2.5, "2.5"},
		{1e21, "1e+21"},
		{1e-7, "1e-7"}, // strconv writes 1e-07
		{1e-6, "0.000001"},
		{0.1, "0.1"},
		{5e-324, "5e-324"},
		{1.7976931348623157e308, "1.7976931348623157e+308"},
		{-1.5e-9, "-1.5e-9"},
		{9007199254740991, "9007199254740991"},
		{1000000, "1000000"},
	}

	for _, c := range cases {
		got, err := Canonicalize(c.value)
		if err != nil {
			t.Errorf("Canonicalize(%v): %v", c.value, err)
			continue
		}
		if got != c.want {
			t.Errorf("Canonicalize(%v) = %s, want %s", c.value, got, c.want)
		}
	}
}

func TestNonFiniteIsRefused(t *testing.T) {
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, err := Canonicalize(value); err == nil {
			t.Errorf("Canonicalize(%v) should have failed", value)
		}
	}
}

func TestHTMLIsNotEscaped(t *testing.T) {
	// encoding/json writes < here, and that alone would put every Go
	// server's hashes in a different bucket from every other SDK's.
	got, err := Canonicalize(map[string]any{"s": "a<b>c&d"})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"s":"a<b>c&d"}`; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestKeysSortByUTF16CodeUnit(t *testing.T) {
	// U+1F680 is the surrogate pair D83D DE80, so it sorts before U+FFFD.
	// Go's natural byte order puts it after.
	got, err := Canonicalize(map[string]any{"�": 1.0, "\U0001F680": 2.0, "é": 3.0, "a": 4.0})
	if err != nil {
		t.Fatal(err)
	}
	want := "{\"a\":4,\"é\":3,\"\U0001F680\":2,\"�\":1}"
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestStructsRoundTripThroughJSON(t *testing.T) {
	type tool struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	got, err := Canonicalize(tool{Query: "a<b", Limit: 25})
	if err != nil {
		t.Fatal(err)
	}
	// The HTML escaping encoding/json applies on the way out must not survive
	// the decode on the way back in.
	if want := `{"limit":25,"query":"a<b"}`; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestArgsHash(t *testing.T) {
	t.Run("absent arguments are the empty object", func(t *testing.T) {
		// A no-argument tool is an ordinary call. Sharing the failure sentinel
		// would make every such tool look broken.
		if ArgsHash(nil) != ArgsHash(map[string]any{}) {
			t.Error("nil and {} should hash the same")
		}
		if ArgsHash(nil) == Unhashable {
			t.Error("nil must not get the failure sentinel")
		}
	})

	t.Run("unserialisable arguments get the sentinel", func(t *testing.T) {
		if got := ArgsHash(map[string]any{"n": math.NaN()}); got != Unhashable {
			t.Errorf("got %s, want %s", got, Unhashable)
		}
		if got := ArgsHash(make(chan int)); got != Unhashable {
			t.Errorf("got %s, want %s", got, Unhashable)
		}
	})

	t.Run("array order still matters", func(t *testing.T) {
		a := ArgsHash(map[string]any{"ids": []any{1.0, 2.0}})
		b := ArgsHash(map[string]any{"ids": []any{2.0, 1.0}})
		if a == b {
			t.Error("reordered arrays are a different call")
		}
	})

	t.Run("is twelve lowercase hex", func(t *testing.T) {
		got := ArgsHash(map[string]any{"q": "anything"})
		if len(got) != 12 || got != strings.ToLower(got) {
			t.Errorf("got %q", got)
		}
	})
}

func TestArgsHashRawMatchesArgsHash(t *testing.T) {
	// The wire path hashes the raw bytes; both must agree.
	raw := json.RawMessage(`{"b":2,"a":1}`)
	if ArgsHashRaw(raw) != ArgsHash(map[string]any{"a": 1.0, "b": 2.0}) {
		t.Error("raw and decoded paths disagree")
	}
	if ArgsHashRaw(nil) != ArgsHash(nil) {
		t.Error("absent arguments should agree on both paths")
	}
}

func TestNewSessionID(t *testing.T) {
	first := NewSessionID()
	if !strings.HasPrefix(first, "s_") || len(first) != 14 {
		t.Errorf("got %q", first)
	}
	if first == NewSessionID() {
		t.Error("session ids must not repeat")
	}
}
