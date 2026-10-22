package database

import (
	"bytes"
	"encoding/hex"
	"testing"
)

func TestCanonicalValueGolden(t *testing.T) {
	tests := []struct {
		name  string
		value canonicalValue
		want  string
	}{
		{
			name:  "inline",
			value: canonicalValue{version: canonicalVersionInline, projection: []byte{1, 2}, record: []byte("r")},
			want:  "d1000100000002010272",
		},
		{
			name:  "external",
			value: canonicalValue{version: canonicalVersionExternal, record: []byte("r")},
			want:  "d100020000000072",
		},
		{
			name:  "inline-tombstone",
			value: canonicalValue{tombstone: true, version: canonicalVersionInline, projection: []byte{1}, record: []byte("r")},
			want:  "d10101000000010172",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := encodeCanonicalValue(tc.value)
			if err != nil {
				t.Fatal(err)
			}
			if got := hex.EncodeToString(raw); got != tc.want {
				t.Fatalf("golden = %s, want %s", got, tc.want)
			}
			decoded, err := decodeCanonicalValue(raw)
			if err != nil {
				t.Fatal(err)
			}
			if decoded.tombstone != tc.value.tombstone || decoded.version != tc.value.version ||
				!bytes.Equal(decoded.projection, tc.value.projection) || !bytes.Equal(decoded.record, tc.value.record) {
				t.Fatalf("roundtrip = %+v, want %+v", decoded, tc.value)
			}
		})
	}
}

func TestCanonicalValueRejectsMalformed(t *testing.T) {
	valid, err := encodeCanonicalValue(canonicalValue{version: canonicalVersionInline, record: []byte("r")})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		raw  []byte
	}{
		{name: "short", raw: valid[:3]},
		{name: "magic", raw: append([]byte{0}, valid[1:]...)},
		{name: "version", raw: append(append([]byte(nil), valid[:2]...), append([]byte{3}, valid[3:]...)...)},
		{name: "flags", raw: append(append([]byte(nil), valid[:1]...), append([]byte{2}, valid[2:]...)...)},
		{name: "projection-length", raw: append(append([]byte(nil), valid[:3]...), []byte{0, 0, 0, 9}...)},
		{name: "external-projection", raw: []byte{canonicalMagic, 0, canonicalVersionExternal, 0, 0, 0, 1, 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decodeCanonicalValue(tc.raw); err == nil {
				t.Fatal("malformed canonical value accepted")
			}
		})
	}
}
