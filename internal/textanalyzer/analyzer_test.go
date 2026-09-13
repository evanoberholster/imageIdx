package textanalyzer

import (
	"slices"
	"testing"
)

func TestAnalyze(t *testing.T) {
	analyzer, err := New(CodeLatin, Version, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := analyzer.Analyze("Café au lait!", nil)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	want := []string{"cafe", "au", "lait"}
	if !slices.Equal(got, want) {
		t.Fatalf("Analyze = %v, want %v", got, want)
	}
	got, err = analyzer.Analyze("Rock'n'roll 2024", nil)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	want = []string{"rock", "n", "roll", "2024"}
	if !slices.Equal(got, want) {
		t.Fatalf("Analyze = %v, want %v", got, want)
	}
}

func TestTokenSetDeduplicates(t *testing.T) {
	analyzer, err := New(CodeLatin, Version, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := analyzer.TokenSet("Paris paris PARIS", nil)
	if err != nil {
		t.Fatalf("TokenSet: %v", err)
	}
	if !slices.Equal(got, []string{"paris"}) {
		t.Fatalf("TokenSet = %v", got)
	}
}

func TestNewRejectsUnknownCodec(t *testing.T) {
	if _, err := New(99, Version, nil); err != ErrUnsupportedCodec {
		t.Fatalf("code error = %v", err)
	}
	if _, err := New(CodeLatin, 99, nil); err != ErrUnsupportedCodec {
		t.Fatalf("version error = %v", err)
	}
	if _, err := New(CodeLatin, Version, []byte{1}); err != ErrUnsupportedCodec {
		t.Fatalf("params error = %v", err)
	}
}
