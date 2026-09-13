// Package textanalyzer implements the small, deterministic text pipeline used
// by bitmapidx text fields: Latin/whitespace tokenization, lowercasing and
// accent folding. It intentionally has no external dependencies and no
// stemming; unsupported codec versions fail closed.
package textanalyzer

import (
	"errors"
	"unicode"
	"unicode/utf8"
)

const (
	// CodeLatin is the only supported analyzer codec in v3.
	CodeLatin uint16 = 1
	// Version is the current analyzer version.
	Version uint16 = 1
)

var (
	// ErrUnsupportedCodec reports an unknown analyzer code, version or params.
	ErrUnsupportedCodec = errors.New("textanalyzer: unsupported codec")
	// ErrTooManyTokens bounds one analysis result.
	ErrTooManyTokens = errors.New("textanalyzer: too many tokens")
)

// MaxTokens bounds tokens produced by one analysis.
const MaxTokens = 4096

// Analyzer tokenizes and normalizes text. It is safe for concurrent use.
type Analyzer struct {
	fold bool
}

// New validates a codec definition and returns its analyzer.
func New(code, version uint16, params []byte) (*Analyzer, error) {
	if code != CodeLatin || version != Version || len(params) != 0 {
		return nil, ErrUnsupportedCodec
	}
	return &Analyzer{fold: true}, nil
}

// Analyze appends normalized tokens to dst and returns the extended slice.
// Letters and digits form tokens; every other rune separates. Tokens are
// lowercased and Latin accents are folded to ASCII.
func (a *Analyzer) Analyze(text string, dst []string) ([]string, error) {
	var buf []byte
	flush := func() error {
		if len(buf) == 0 {
			return nil
		}
		if len(dst) >= MaxTokens {
			return ErrTooManyTokens
		}
		dst = append(dst, string(buf))
		buf = buf[:0]
		return nil
	}
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			r = unicode.ToLower(r)
			if a.fold {
				r = foldLatin(r)
			}
			buf = utf8.AppendRune(buf, r)
			continue
		}
		if err := flush(); err != nil {
			return dst, err
		}
	}
	if err := flush(); err != nil {
		return dst, err
	}
	return dst, nil
}

// TokenSet analyzes text and returns its deduplicated tokens.
func (a *Analyzer) TokenSet(text string, dst []string) ([]string, error) {
	dst, err := a.Analyze(text, dst)
	if err != nil {
		return dst, err
	}
	return Dedup(dst), nil
}

// Dedup removes duplicate tokens in place while preserving first-seen order.
func Dedup(tokens []string) []string {
	if len(tokens) < 2 {
		return tokens
	}
	seen := make(map[string]struct{}, len(tokens))
	out := tokens[:0]
	for _, token := range tokens {
		if _, ok := seen[token]; ok {
			continue
		}
		seen[token] = struct{}{}
		out = append(out, token)
	}
	return out
}

// foldLatin maps common Latin accented letters to ASCII. Non-Latin scripts
// are returned unchanged.
func foldLatin(r rune) rune {
	if folded, ok := latinFold[r]; ok {
		return folded
	}
	return r
}

var latinFold = map[rune]rune{
	'À': 'A', 'Á': 'A', 'Â': 'A', 'Ã': 'A', 'Ä': 'A', 'Å': 'A', 'Æ': 'A',
	'Ç': 'C', 'È': 'E', 'É': 'E', 'Ê': 'E', 'Ë': 'E', 'Ì': 'I', 'Í': 'I',
	'Î': 'I', 'Ï': 'I', 'Ð': 'D', 'Ñ': 'N', 'Ò': 'O', 'Ó': 'O', 'Ô': 'O',
	'Õ': 'O', 'Ö': 'O', 'Ø': 'O', 'Ù': 'U', 'Ú': 'U', 'Û': 'U', 'Ü': 'U',
	'Ý': 'Y', 'Þ': 'T', 'ß': 's',
	'à': 'a', 'á': 'a', 'â': 'a', 'ã': 'a', 'ä': 'a', 'å': 'a', 'æ': 'a',
	'ç': 'c', 'è': 'e', 'é': 'e', 'ê': 'e', 'ë': 'e', 'ì': 'i', 'í': 'i',
	'î': 'i', 'ï': 'i', 'ð': 'd', 'ñ': 'n', 'ò': 'o', 'ó': 'o', 'ô': 'o',
	'õ': 'o', 'ö': 'o', 'ø': 'o', 'ù': 'u', 'ú': 'u', 'û': 'u', 'ü': 'u',
	'ý': 'y', 'þ': 't', 'ÿ': 'y',
	'Ā': 'A', 'ā': 'a', 'Ă': 'A', 'ă': 'a', 'Ą': 'A', 'ą': 'a', 'Ć': 'C',
	'ć': 'c', 'Ĉ': 'C', 'ĉ': 'c', 'Ċ': 'C', 'ċ': 'c', 'Č': 'C', 'č': 'c',
	'Ď': 'D', 'ď': 'd', 'Đ': 'D', 'đ': 'd', 'Ē': 'E', 'ē': 'e', 'Ĕ': 'E',
	'ĕ': 'e', 'Ė': 'E', 'ė': 'e', 'Ę': 'E', 'ę': 'e', 'Ě': 'E', 'ě': 'e',
	'Ĝ': 'G', 'ĝ': 'g', 'Ğ': 'G', 'ğ': 'g', 'Ġ': 'G', 'ġ': 'g', 'Ģ': 'G',
	'ģ': 'g', 'Ĥ': 'H', 'ĥ': 'h', 'Ħ': 'H', 'ħ': 'h', 'Ĩ': 'I', 'ĩ': 'i',
	'Ī': 'I', 'ī': 'i', 'Ĭ': 'I', 'ĭ': 'i', 'Į': 'I', 'į': 'i', 'İ': 'I',
	'ı': 'i', 'Ĵ': 'J', 'ĵ': 'j', 'Ķ': 'K', 'ķ': 'k', 'Ĺ': 'L', 'ĺ': 'l',
	'Ļ': 'L', 'ļ': 'l', 'Ľ': 'L', 'ľ': 'l', 'Ł': 'L', 'ł': 'l', 'Ń': 'N',
	'ń': 'n', 'Ņ': 'N', 'ņ': 'n', 'Ň': 'N', 'ň': 'n', 'Ŋ': 'N', 'ŋ': 'n',
	'Ō': 'O', 'ō': 'o', 'Ŏ': 'O', 'ŏ': 'o', 'Ő': 'O', 'ő': 'o', 'Œ': 'O',
	'œ': 'o', 'Ŕ': 'R', 'ŕ': 'r', 'Ŗ': 'R', 'ŗ': 'r', 'Ř': 'R', 'ř': 'r',
	'Ś': 'S', 'ś': 's', 'Ŝ': 'S', 'ŝ': 's', 'Ş': 'S', 'ş': 's', 'Š': 'S',
	'š': 's', 'Ţ': 'T', 'ţ': 't', 'Ť': 'T', 'ť': 't', 'Ŧ': 'T', 'ŧ': 't',
	'Ũ': 'U', 'ũ': 'u', 'Ū': 'U', 'ū': 'u', 'Ŭ': 'U', 'ŭ': 'u', 'Ů': 'U',
	'ů': 'u', 'Ű': 'U', 'ű': 'u', 'Ų': 'U', 'ų': 'u', 'Ŵ': 'W', 'ŵ': 'w',
	'Ŷ': 'Y', 'ŷ': 'y', 'Ÿ': 'Y', 'Ź': 'Z', 'ź': 'z', 'Ż': 'Z', 'ż': 'z',
	'Ž': 'Z', 'ž': 'z',
}
