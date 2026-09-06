package mcpulse

// JSON Canonicalization Scheme (RFC 8785).
//
// ArgsHash only means anything if every MCPulse SDK, in every language, turns
// the same arguments into the same bytes. encoding/json does not get there on
// its own: it HTML-escapes <, > and & by default, it sorts map keys by UTF-8
// bytes where JCS sorts by UTF-16 code unit, and its float formatting is not
// ECMAScript's. Each of those silently sends the same call to a different
// bucket than the TypeScript SDK would.
//
// So none of the serialisation below goes through encoding/json. Every rule is
// spelled out, and testdata/canonical.json — the same file the TypeScript SDK
// runs — is what holds this file to them.

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// ErrNotJSON is returned for anything JSON cannot represent: NaN, an infinity,
// a channel, a cycle.
var ErrNotJSON = errors.New("mcpulse: value cannot be represented as JSON")

// maxDepth bounds recursion. A cyclic structure reached through interface
// values cannot be detected cheaply by identity, so depth is what stops it —
// and no real tool argument is a thousand levels deep.
const maxDepth = 1000

// Canonicalize returns the RFC 8785 canonical JSON form of v.
//
// It is written for the shapes encoding/json produces when decoding into any —
// map[string]any, []any, float64, string, bool, nil — which is what arrives
// from the wire. Other Go values are marshalled to JSON and re-read first, so
// a struct canonicalises as the JSON it would have become.
func Canonicalize(v any) (string, error) {
	var b strings.Builder
	if err := write(&b, v, 0); err != nil {
		return "", err
	}
	return b.String(), nil
}

func write(b *strings.Builder, v any, depth int) error {
	if depth > maxDepth {
		return fmt.Errorf("%w: nested too deeply", ErrNotJSON)
	}

	switch value := v.(type) {
	case nil:
		b.WriteString("null")
		return nil

	case bool:
		if value {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
		return nil

	case string:
		writeString(b, value)
		return nil

	case float64:
		return writeNumber(b, value)
	case float32:
		return writeNumber(b, float64(value))
	case int:
		return writeNumber(b, float64(value))
	case int8:
		return writeNumber(b, float64(value))
	case int16:
		return writeNumber(b, float64(value))
	case int32:
		return writeNumber(b, float64(value))
	case int64:
		return writeNumber(b, float64(value))
	case uint:
		return writeNumber(b, float64(value))
	case uint8:
		return writeNumber(b, float64(value))
	case uint16:
		return writeNumber(b, float64(value))
	case uint32:
		return writeNumber(b, float64(value))
	case uint64:
		return writeNumber(b, float64(value))

	case json.Number:
		parsed, err := value.Float64()
		if err != nil {
			return fmt.Errorf("%w: %v", ErrNotJSON, err)
		}
		return writeNumber(b, parsed)

	case []any:
		b.WriteByte('[')
		for i, item := range value {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := write(b, item, depth+1); err != nil {
				return err
			}
		}
		b.WriteByte(']')
		return nil

	case map[string]any:
		b.WriteByte('{')
		for i, key := range sortedKeys(value) {
			if i > 0 {
				b.WriteByte(',')
			}
			writeString(b, key)
			b.WriteByte(':')
			if err := write(b, value[key], depth+1); err != nil {
				return err
			}
		}
		b.WriteByte('}')
		return nil
	}

	// Anything else — a struct, a named map, a pointer. Round-trip it through
	// encoding/json so it arrives here as one of the shapes above. The escaping
	// encoding/json applies on the way out is undone by the decode on the way
	// back in, so it cannot leak into the canonical form.
	return writeViaJSON(b, v, depth)
}

func writeViaJSON(b *strings.Builder, v any, depth int) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNotJSON, err)
	}

	var decoded any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	// UseNumber would defeat the point: JCS is defined over IEEE-754 doubles,
	// and float64 is what the wire decode produces anyway.
	if err := decoder.Decode(&decoded); err != nil {
		return fmt.Errorf("%w: %v", ErrNotJSON, err)
	}

	switch decoded.(type) {
	case map[string]any, []any, string, float64, bool, nil:
		return write(b, decoded, depth+1)
	default:
		return fmt.Errorf("%w: %T", ErrNotJSON, decoded)
	}
}

// sortedKeys orders keys by their UTF-16 code units, as RFC 8785 §3.2.3 asks.
//
// Go's natural order is by UTF-8 bytes, and the two disagree above the Basic
// Multilingual Plane: U+1F680 encodes as the surrogate pair D83D DE80, which
// sorts before U+FFFD in UTF-16 and after it in UTF-8.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		return lessUTF16(keys[i], keys[j])
	})
	return keys
}

func lessUTF16(a, b string) bool {
	// Fast path: below U+10000 the two orders agree, so the common case never
	// allocates.
	if isBMP(a) && isBMP(b) {
		return a < b
	}

	left := utf16.Encode([]rune(a))
	right := utf16.Encode([]rune(b))
	for i := 0; i < len(left) && i < len(right); i++ {
		if left[i] != right[i] {
			return left[i] < right[i]
		}
	}
	return len(left) < len(right)
}

func isBMP(s string) bool {
	for _, r := range s {
		if r > 0xFFFF {
			return false
		}
	}
	return true
}

// ─── Strings ─────────────────────────────────────────────────────────────────

const hexDigits = "0123456789abcdef"

// writeString emits a JSON string per JCS §3.2.2.2, which is ECMAScript's
// escaping: the short escapes where one exists, lowercase \u00xx for the rest
// of the C0 range, and nothing else touched.
//
// In particular <, > and & are written literally. encoding/json escapes them by
// default, and that alone would put every Go server's hashes in a different
// bucket from every other SDK's.
func writeString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			switch {
			case r < 0x20:
				b.WriteString(`\u00`)
				b.WriteByte(hexDigits[(r>>4)&0xF])
				b.WriteByte(hexDigits[r&0xF])
			case r == utf8.RuneError:
				// A byte sequence that was not valid UTF-8. Go's decoder
				// already replaced it, and U+FFFD is what any other SDK would
				// have received over the wire too.
				b.WriteRune(r)
			default:
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

// ─── Numbers ─────────────────────────────────────────────────────────────────

// writeNumber emits ECMAScript's Number::toString, which JCS §3.2.2.3 defers to.
//
// Go's own formatting is close but not equal: strconv writes 1e-07 where
// ECMAScript writes 1e-7, and 1e+21 has to stay in exponent form while
// 1000000 must not enter it.
func writeNumber(b *strings.Builder, f float64) error {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		// Not JSON. Coercing to null the way some encoders do would hand two
		// genuinely different calls the same hash.
		return fmt.Errorf("%w: non-finite number", ErrNotJSON)
	}

	if f == 0 {
		// Covers negative zero, which JCS writes as "0".
		b.WriteString("0")
		return nil
	}

	if f < 0 {
		b.WriteByte('-')
		f = -f
	}

	digits, n := shortest(f)
	k := len(digits)

	// The five cases of ECMAScript Number::toString, in its own order.
	switch {
	case k <= n && n <= 21:
		b.WriteString(digits)
		b.WriteString(strings.Repeat("0", n-k))
	case 0 < n && n <= 21:
		b.WriteString(digits[:n])
		b.WriteByte('.')
		b.WriteString(digits[n:])
	case -6 < n && n <= 0:
		b.WriteString("0.")
		b.WriteString(strings.Repeat("0", -n))
		b.WriteString(digits)
	default:
		exponent := n - 1
		if k == 1 {
			b.WriteString(digits)
		} else {
			b.WriteString(digits[:1])
			b.WriteByte('.')
			b.WriteString(digits[1:])
		}
		b.WriteByte('e')
		if exponent >= 0 {
			b.WriteByte('+')
		} else {
			b.WriteByte('-')
			exponent = -exponent
		}
		b.WriteString(strconv.Itoa(exponent))
	}
	return nil
}

// shortest decomposes a positive finite float into (digits, n): the shortest
// decimal that round-trips, with no leading or trailing zeros, and the position
// of the decimal point. The value is digits * 10**(n-len(digits)).
func shortest(f float64) (string, int) {
	// 'e' with precision -1 is Go's shortest round-tripping form, and it always
	// has the shape d[.ddd]e±dd, which is the easiest to take apart.
	formatted := strconv.FormatFloat(f, 'e', -1, 64)

	mantissa, exponentText, found := strings.Cut(formatted, "e")
	if !found {
		return "0", 1
	}

	exponent, err := strconv.Atoi(exponentText)
	if err != nil {
		return "0", 1
	}

	digits := strings.Replace(mantissa, ".", "", 1)
	digits = strings.TrimRight(digits, "0")
	if digits == "" {
		digits = "0"
	}

	// The mantissa is always a single digit before the point, so the decimal
	// point sits one place further right than the exponent says.
	return digits, exponent + 1
}
