package credgw

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"strings"
	"testing"
)

// referenceRedactingWriter is the byte-at-a-time credentialRedactingWriter
// as it stood before the fast path (gitmoot#2695). It is the oracle for
// TestCredentialRedactingWriterMatchesReference; keep it frozen.
type referenceRedactingWriter struct {
	destination io.Writer
	patterns    []credentialRedactionPattern
	pending     []byte
}

func (w *referenceRedactingWriter) Write(data []byte) (int, error) {
	for _, value := range data {
		w.pending = append(w.pending, value)
		for len(w.pending) > 0 {
			matchLength, couldGrow := w.match()
			if matchLength > 0 && !couldGrow {
				if _, err := io.WriteString(w.destination, "[REDACTED]"); err != nil {
					return 0, err
				}
				w.pending = w.pending[matchLength:]
				continue
			}
			if couldGrow {
				break
			}
			if _, err := w.destination.Write(w.pending[:1]); err != nil {
				return 0, err
			}
			w.pending = w.pending[1:]
		}
	}
	return len(data), nil
}

func (w *referenceRedactingWriter) match() (matchLength int, couldGrow bool) {
	for _, pattern := range w.patterns {
		if referencePatternPrefix(pattern, w.pending) && len(w.pending) < len(pattern.value) {
			couldGrow = true
		}
		if len(w.pending) >= len(pattern.value) && referencePatternPrefix(pattern, w.pending[:len(pattern.value)]) && len(pattern.value) > matchLength {
			matchLength = len(pattern.value)
		}
	}
	return matchLength, couldGrow
}

func referencePatternPrefix(pattern credentialRedactionPattern, prefix []byte) bool {
	if len(prefix) > len(pattern.value) {
		return false
	}
	want := pattern.value[:len(prefix)]
	if pattern.asciiFold {
		return bytes.EqualFold(want, prefix)
	}
	return bytes.Equal(want, prefix)
}

func (w *referenceRedactingWriter) flush() error {
	for len(w.pending) > 0 {
		matchLength, _ := w.match()
		if matchLength > 0 {
			if _, err := io.WriteString(w.destination, "[REDACTED]"); err != nil {
				return err
			}
			w.pending = w.pending[matchLength:]
			continue
		}
		if _, err := w.destination.Write(w.pending[:1]); err != nil {
			return err
		}
		w.pending = w.pending[1:]
	}
	return nil
}

var errRedactorBudget = errors.New("destination budget exhausted")

// budgetWriter accepts at most budget bytes in total (budget < 0 means
// unlimited), storing the accepted prefix of a refused write, so a byte-wise
// and a batched writer leave identical contents when it fails.
type budgetWriter struct {
	budget int
	output bytes.Buffer
	calls  int
}

func (w *budgetWriter) Write(data []byte) (int, error) {
	w.calls++
	if w.budget < 0 || len(data) <= w.budget-w.output.Len() {
		return w.output.Write(data)
	}
	accepted := w.budget - w.output.Len()
	w.output.Write(data[:accepted])
	return accepted, errRedactorBudget
}

// redactorFixedSecrets cover ASCII and fold-sensitive letters, characters every
// escaping pattern rewrites, Unicode case folds (Kelvin sign, long s), invalid
// UTF-8 and one-byte secrets whose patterns complete on their first byte.
var redactorFixedSecrets = []string{
	testRealCredential,
	"ghp_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789",
	"a b/c?d&e=f%g+h\"i\\j\n",
	"ключ-secret-\u212Aelvin-\u017Fs",
	"\u212Aey\u017F",
	"kss",
	"\xff\xfe-raw\x80",
	"\x80",
	"x",
	"Ab",
	"%",
	"=",
	"AAAA",
}

const redactorSecretAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_.~%+/= \"\\\n"

func randomRedactorSecret(random *rand.Rand) string {
	length := 1 + random.IntN(24)
	secret := make([]byte, length)
	mode := random.IntN(4)
	for i := range secret {
		switch {
		case mode == 0 || (mode == 3 && random.IntN(4) == 0):
			secret[i] = byte(random.IntN(256))
		case mode == 1:
			secret[i] = "abAB01"[random.IntN(6)]
		default:
			secret[i] = redactorSecretAlphabet[random.IntN(len(redactorSecretAlphabet))]
		}
	}
	return string(secret)
}

func randomRedactorPatterns(random *rand.Rand) []credentialRedactionPattern {
	var secret string
	if random.IntN(3) == 0 {
		secret = redactorFixedSecrets[random.IntN(len(redactorFixedSecrets))]
	} else {
		secret = randomRedactorSecret(random)
	}
	var extra []string
	for range random.IntN(3) {
		switch random.IntN(4) {
		case 0:
			extra = append(extra, secret[:1+random.IntN(len(secret))])
		case 1:
			extra = append(extra, secret[random.IntN(len(secret)):])
		case 2:
			// Fold-equal to the secret but of another byte length.
			extra = append(extra, string(unicodeFoldVariant(random, flipASCIICase(random, []byte(secret)))))
		default:
			extra = append(extra, randomRedactorSecret(random))
		}
	}
	return responseCredentialPatterns(secret, extra)
}

func flipASCIICase(random *rand.Rand, value []byte) []byte {
	flipped := bytes.Clone(value)
	for i, character := range flipped {
		if lower := character | 0x20; 'a' <= lower && lower <= 'z' && random.IntN(2) == 0 {
			flipped[i] = character ^ 0x20
		}
	}
	return flipped
}

func unicodeFoldVariant(random *rand.Rand, value []byte) []byte {
	var folded []byte
	for _, character := range value {
		switch {
		case (character == 'k' || character == 'K') && random.IntN(2) == 0:
			folded = append(folded, "\u212A"...)
		case (character == 's' || character == 'S') && random.IntN(2) == 0:
			folded = append(folded, "\u017F"...)
		default:
			folded = append(folded, character)
		}
	}
	return folded
}

func randomRedactorInput(random *rand.Rand, patterns []credentialRedactionPattern) []byte {
	var alphabet []byte
	for _, pattern := range patterns {
		alphabet = append(alphabet, pattern.value...)
	}
	pick := func() []byte {
		if len(patterns) == 0 {
			return []byte("no-patterns")
		}
		return patterns[random.IntN(len(patterns))].value
	}
	var input []byte
	for range random.IntN(14) {
		value := pick()
		switch random.IntN(12) {
		case 0:
			for range random.IntN(48) {
				input = append(input, byte(random.IntN(256)))
			}
		case 1:
			const text = "lorem ipsum {\"k\":[1,2]}\n"
			for range random.IntN(48) {
				input = append(input, text[random.IntN(len(text))])
			}
		case 2:
			// Noise from the patterns' own bytes produces many near misses.
			for range random.IntN(48) {
				if len(alphabet) > 0 {
					input = append(input, alphabet[random.IntN(len(alphabet))])
				}
			}
		case 3:
			input = append(input, value...)
		case 4:
			input = append(input, flipASCIICase(random, value)...)
		case 5:
			input = append(input, unicodeFoldVariant(random, flipASCIICase(random, value))...)
		case 6:
			input = append(input, value[:random.IntN(len(value)+1)]...)
		case 7:
			// Overlap: a partial occurrence running into a full one.
			input = append(input, value[:random.IntN(len(value)+1)]...)
			input = append(input, value...)
		case 8:
			for range 1 + random.IntN(3) {
				input = append(input, value...)
			}
			input = append(input, value[random.IntN(len(value)+1):]...)
		case 9:
			mutated := bytes.Clone(value)
			if len(mutated) > 0 {
				mutated[random.IntN(len(mutated))] ^= byte(1 + random.IntN(255))
			}
			input = append(input, mutated...)
		case 10:
			other := pick()
			input = append(input, value[:random.IntN(len(value)+1)]...)
			input = append(input, other...)
		default:
		}
	}
	if len(patterns) > 0 && random.IntN(3) == 0 {
		value := flipASCIICase(random, pick())
		input = append(input, value[:random.IntN(len(value)+1)]...)
	}
	return input
}

func randomRedactorChunks(random *rand.Rand, input []byte) [][]byte {
	var chunks [][]byte
	switch random.IntN(5) {
	case 0:
		for i := range input {
			chunks = append(chunks, input[i:i+1])
		}
	case 1:
		chunks = append(chunks, input)
	default:
		for start := 0; start < len(input); {
			end := min(len(input), start+1+random.IntN(1+random.IntN(len(input))))
			chunks = append(chunks, input[start:end])
			start = end
		}
	}
	if random.IntN(2) == 0 {
		for range 1 + random.IntN(3) {
			at := random.IntN(len(chunks) + 1)
			chunks = append(chunks[:at], append([][]byte{{}}, chunks[at:]...)...)
		}
	}
	return chunks
}

func TestCredentialRedactingWriterFirstByteFiltersMatchPatternPrefix(t *testing.T) {
	// bytes.EqualFold folds U+212A KELVIN SIGN onto k, so a folded pattern can
	// match a prefix whose first byte is unrelated to its own.
	kelvin := credentialRedactionPattern{value: []byte("\u212Ass"), asciiFold: true}
	if !referencePatternPrefix(kelvin, []byte("k\u017F\u017F")) || !patternMayStartWith(&kelvin, 'k') {
		t.Fatal("Kelvin-sign pattern must stay a candidate for a prefix starting with k")
	}
	random := rand.New(rand.NewPCG(2695, 1))
	sets := [][]credentialRedactionPattern{nil, {{value: nil, asciiFold: true}}, {kelvin}}
	for _, secret := range redactorFixedSecrets {
		sets = append(sets, credentialRedactionPatterns(secret))
	}
	for range 500 {
		sets = append(sets, randomRedactorPatterns(random))
	}
	for _, patterns := range sets {
		writer := newCredentialRedactingWriter(io.Discard, patterns)
		for value := range 256 {
			want := false
			for _, pattern := range patterns {
				want = want || referencePatternPrefix(pattern, []byte{byte(value)})
			}
			if writer.startsMatch[value] != want {
				t.Fatalf("startsMatch[%#x]=%v, patternPrefix says %v", value, writer.startsMatch[value], want)
			}
		}
		var prefixes [][]byte
		for _, pattern := range patterns {
			prefixes = append(prefixes, pattern.value, flipASCIICase(random, pattern.value), unicodeFoldVariant(random, flipASCIICase(random, pattern.value)))
		}
		for _, pattern := range patterns {
			if len(pattern.value) == 0 {
				continue
			}
			for _, prefix := range prefixes {
				for length := 1; length <= min(len(prefix), len(pattern.value)); length++ {
					if referencePatternPrefix(pattern, prefix[:length]) && !patternMayStartWith(&pattern, prefix[0]) {
						t.Fatalf("patternMayStartWith(%q, %#x) ruled out matching prefix %q", pattern.value, prefix[0], prefix[:length])
					}
				}
			}
		}
	}
}

// compareWithReference feeds chunks to the reference and the fast writer over
// destinations failing after budget bytes, requiring identical returns,
// destination contents after every call, and carry-over.
func compareWithReference(t *testing.T, label string, patterns []credentialRedactionPattern, chunks [][]byte, budget int) []byte {
	t.Helper()
	referenceOutput := &budgetWriter{budget: budget}
	reference := &referenceRedactingWriter{destination: referenceOutput, patterns: patterns}
	fastOutput := &budgetWriter{budget: budget}
	fast := newCredentialRedactingWriter(fastOutput, patterns)
	for call, chunk := range chunks {
		wantN, wantErr := reference.Write(chunk)
		gotN, gotErr := fast.Write(chunk)
		if gotN != wantN || gotErr != wantErr {
			t.Fatalf("%s: write %d returned (%d, %v), reference (%d, %v)", label, call, gotN, gotErr, wantN, wantErr)
		}
		if !bytes.Equal(fastOutput.output.Bytes(), referenceOutput.output.Bytes()) {
			t.Fatalf("%s: after write %d output %q, reference %q", label, call, fastOutput.output.Bytes(), referenceOutput.output.Bytes())
		}
		if wantErr != nil {
			return referenceOutput.output.Bytes()
		}
		if !bytes.Equal(fast.pending, reference.pending) {
			t.Fatalf("%s: after write %d pending %q, reference %q", label, call, fast.pending, reference.pending)
		}
	}
	wantErr := reference.flush()
	gotErr := fast.flush()
	if gotErr != wantErr {
		t.Fatalf("%s: flush returned %v, reference %v", label, gotErr, wantErr)
	}
	if !bytes.Equal(fastOutput.output.Bytes(), referenceOutput.output.Bytes()) {
		t.Fatalf("%s: final output %q, reference %q", label, fastOutput.output.Bytes(), referenceOutput.output.Bytes())
	}
	return referenceOutput.output.Bytes()
}

func TestCredentialRedactingWriterMatchesReference(t *testing.T) {
	// Fixed edge: "xkſſ" grows pending through the first secret, so after
	// 'x' is emitted the pending "kſſ" fold-matches the Kelvin-sign secret,
	// although 'k' alone could never start a match.
	kelvin := responseCredentialPatterns("xk\u017F\u017Fz", []string{"\u212Ass"})
	kelvinInput := []byte("xk\u017F\u017Fy")
	for _, chunks := range [][][]byte{{kelvinInput}, {kelvinInput[:2], kelvinInput[2:]}} {
		if output := compareWithReference(t, "kelvin", kelvin, chunks, -1); string(output) != "x[REDACTED]y" {
			t.Fatalf("kelvin edge output %q, want x[REDACTED]y", output)
		}
	}

	const cases = 6000
	random := rand.New(rand.NewPCG(2695, 2695))
	for index := range cases {
		patterns := randomRedactorPatterns(random)
		input := randomRedactorInput(random, patterns)
		chunks := randomRedactorChunks(random, input)
		budget := -1
		if index%4 == 0 {
			// Fail the destination part way through the full output.
			budget = random.IntN(len(redactCredentialText(string(input), patterns)) + 1)
		}
		label := fmt.Sprintf("case %d patterns=%d input=%q chunks=%d budget=%d", index, len(patterns), input, len(chunks), budget)
		compareWithReference(t, label, patterns, chunks, budget)
	}
}

func redactorBenchmarkResponse(size int) []byte {
	words := strings.Fields("the review found a race in the scheduler when two jobs share a worktree and the lease expires before the retry so we should hold the lock across both steps")
	var response bytes.Buffer
	for index := 0; response.Len() < size; index++ {
		fmt.Fprintf(&response, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"%s \"}}\n\n", words[index%len(words)])
	}
	return response.Bytes()[:size]
}

func TestCredentialRedactingWriterBatchesDestinationWrites(t *testing.T) {
	patterns := responseCredentialPatterns(testRealCredential, nil)
	response := redactorBenchmarkResponse(256 * 1024)
	// Secrets inside and across chunk boundaries, plus near misses.
	for offset := 1000; offset+len(testRealCredential) < len(response); offset += 7919 {
		copy(response[offset:], testRealCredential)
		copy(response[offset+3000:], testRealCredential[:len(testRealCredential)-1])
	}
	destination := &budgetWriter{budget: -1}
	writer := newCredentialRedactingWriter(destination, patterns)
	writes := 0
	for start := 0; start < len(response); start += 4096 {
		before := destination.calls
		if _, err := writer.Write(response[start:min(len(response), start+4096)]); err != nil {
			t.Fatal(err)
		}
		writes++
		if calls := destination.calls - before; calls > 1 {
			t.Fatalf("write %d made %d destination writes, want at most 1", writes, calls)
		}
	}
	before := destination.calls
	if err := writer.flush(); err != nil {
		t.Fatal(err)
	}
	if calls := destination.calls - before; calls > 1 {
		t.Fatalf("flush made %d destination writes, want at most 1", calls)
	}
	output := destination.output.String()
	if !strings.Contains(output, "[REDACTED]") || strings.Contains(output, testRealCredential) {
		t.Fatalf("redacted output length=%d lacks redaction or leaks the credential", len(output))
	}
}

// BenchmarkCredentialRedactingWriter streams a 1 MiB model response through
// the redactor in 4 KiB chunks, flushing a bufio.Writer per chunk as
// streamRedactedResponse does.
func BenchmarkCredentialRedactingWriter(b *testing.B) {
	const chunkSize = 4 * 1024
	patterns := responseCredentialPatterns(testRealCredential, nil)
	response := redactorBenchmarkResponse(1 << 20)
	buffered := bufio.NewWriter(io.Discard)
	b.SetBytes(int64(len(response)))
	b.ReportAllocs()
	for b.Loop() {
		writer := newCredentialRedactingWriter(buffered, patterns)
		for start := 0; start < len(response); start += chunkSize {
			if _, err := writer.Write(response[start:min(len(response), start+chunkSize)]); err != nil {
				b.Fatal(err)
			}
			if err := buffered.Flush(); err != nil {
				b.Fatal(err)
			}
		}
		if err := writer.flush(); err != nil {
			b.Fatal(err)
		}
		if err := buffered.Flush(); err != nil {
			b.Fatal(err)
		}
	}
}
