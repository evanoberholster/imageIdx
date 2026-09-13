package hashidx

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
)

func mustHex(t *testing.T, s string) Hash {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	var h Hash
	copy(h[:], b)
	return h
}

func TestHashKnownAnswers(t *testing.T) {
	// SHA-256 vectors (FIPS 180-4).
	if got := SumSHA256(nil); got != mustHex(t, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855") {
		t.Fatalf("sha256(\"\") = %x", got)
	}
	if got := SumSHA256([]byte("abc")); got != mustHex(t, "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad") {
		t.Fatalf("sha256(\"abc\") = %x", got)
	}
	// BLAKE3 vectors (reference implementation).
	if got := SumBLAKE3(nil); got != mustHex(t, "af1349b9f5f9a1a6a0404dea36dcc9499bcb25c9adc112b7cc9a93cae41f3262") {
		t.Fatalf("blake3(\"\") = %x", got)
	}
	if got := SumBLAKE3([]byte("abc")); got != mustHex(t, "6437b3ac38465133ffb63b75273a8db548c558465d79db03fd359c6cd5bd9d85") {
		t.Fatalf("blake3(\"abc\") = %x", got)
	}
}

func TestHashReaderMatchesOneshot(t *testing.T) {
	payload := []byte(strings.Repeat("image-bytes-0123456789", 64))
	for _, tc := range []struct {
		name string
		one  func([]byte) Hash
		rd   func(*bytes.Reader) (Hash, error)
	}{
		{"sha256", SumSHA256, func(r *bytes.Reader) (Hash, error) { return HashReaderSHA256(r) }},
		{"blake3", SumBLAKE3, func(r *bytes.Reader) (Hash, error) { return HashReaderBLAKE3(r) }},
	} {
		want := tc.one(payload)
		got, err := tc.rd(bytes.NewReader(payload))
		if err != nil {
			t.Fatalf("%s reader: %v", tc.name, err)
		}
		if got != want {
			t.Fatalf("%s streaming != one-shot", tc.name)
		}
		// Empty stream equals empty one-shot.
		gotEmpty, err := tc.rd(bytes.NewReader(nil))
		if err != nil {
			t.Fatal(err)
		}
		if gotEmpty != tc.one(nil) {
			t.Fatalf("%s empty streaming mismatch", tc.name)
		}
	}
}
