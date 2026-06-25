package telnet

import (
	"bytes"
	"strings"
	"testing"
)

func TestReadLineStripsNegotiationAndCRLF(t *testing.T) {
	// "ab" IAC WILL ECHO "c" CRLF, then "de" LF.
	input := []byte{'a', 'b', iac, will, 1, 'c', '\r', '\n', 'd', 'e', '\n'}
	var out bytes.Buffer
	c := NewReadWriter(bytes.NewReader(input), &out)

	if l, err := c.ReadLine(); err != nil || l != "abc" {
		t.Fatalf("line 1 = %q, %v; want \"abc\"", l, err)
	}
	if l, err := c.ReadLine(); err != nil || l != "de" {
		t.Fatalf("line 2 = %q, %v; want \"de\"", l, err)
	}
	// WILL ECHO must be refused with DONT ECHO.
	if got := out.Bytes(); !bytes.Equal(got, []byte{iac, dont, 1}) {
		t.Fatalf("refusal = % x; want IAC DONT ECHO", got)
	}
}

func TestReadLineEscapedIAC(t *testing.T) {
	// "x" IAC IAC "y" LF  -> literal 0xFF between x and y.
	input := []byte{'x', iac, iac, 'y', '\n'}
	c := NewReadWriter(bytes.NewReader(input), &bytes.Buffer{})
	l, err := c.ReadLine()
	if err != nil || l != "x\xffy" {
		t.Fatalf("line = %q, %v; want \"x\\xffy\"", l, err)
	}
}

func TestWriteEscapesIAC(t *testing.T) {
	var out bytes.Buffer
	c := NewReadWriter(&bytes.Buffer{}, &out)
	if err := c.Write("a\xffb"); err != nil {
		t.Fatal(err)
	}
	if got := out.Bytes(); !bytes.Equal(got, []byte{'a', iac, iac, 'b'}) {
		t.Fatalf("write = % x; want a IAC IAC b", got)
	}
}

// TestReadLineStripsControlChars: ESC, BEL, and backspace are dropped from the
// returned user-input line (terminal-injection defense), while ordinary text is
// preserved. This runs after IAC consumption, on the final assembled line.
func TestReadLineStripsControlChars(t *testing.T) {
	// "say " ESC "[2J" BEL "x" BS "y" LF
	input := []byte{'s', 'a', 'y', ' ', 0x1b, '[', '2', 'J', 0x07, 'x', 0x08, 'y', '\n'}
	c := NewReadWriter(bytes.NewReader(input), &bytes.Buffer{})
	l, err := c.ReadLine()
	if err != nil {
		t.Fatal(err)
	}
	if l != "say [2Jxy" {
		t.Fatalf("line = %q; want %q (controls stripped, brackets/text kept)", l, "say [2Jxy")
	}
	if strings.ContainsAny(l, "\x1b\x07\x08") {
		t.Fatalf("line %q still contains a control char", l)
	}
}

// TestReadLinePreservesMultibyteUTF8: emoji and accented text survive intact —
// the sanitizer is rune-level, so the 0x80-0x9F continuation bytes inside
// multibyte runes are NOT mistaken for C1 controls and stripped.
func TestReadLinePreservesMultibyteUTF8(t *testing.T) {
	const want = "say café 😀 naïve"
	input := append([]byte(want), '\n')
	c := NewReadWriter(bytes.NewReader(input), &bytes.Buffer{})
	l, err := c.ReadLine()
	if err != nil {
		t.Fatal(err)
	}
	if l != want {
		t.Fatalf("line = %q; want %q (multibyte runes must pass through)", l, want)
	}
}

// TestReadLineTabDropped: tab is a control rune and is dropped, per the documented
// tab decision.
func TestReadLineTabDropped(t *testing.T) {
	input := []byte("a\tb\n")
	c := NewReadWriter(bytes.NewReader(input), &bytes.Buffer{})
	l, err := c.ReadLine()
	if err != nil {
		t.Fatal(err)
	}
	if l != "ab" {
		t.Fatalf("line = %q; want %q (tab dropped)", l, "ab")
	}
}

// TestReadLineCapsOversizedLine: a line far over MaxLineBytes is truncated to the
// cap (the read buffer stays bounded), the connection survives, the user is told,
// and the NEXT line parses cleanly — not a torn half of the over-long line.
func TestReadLineCapsOversizedLine(t *testing.T) {
	huge := strings.Repeat("A", MaxLineBytes*4)
	var out bytes.Buffer
	input := []byte(huge + "\n" + "next\n")
	c := NewReadWriter(bytes.NewReader(input), &out)

	l1, err := c.ReadLine()
	if err != nil {
		t.Fatal(err)
	}
	if len(l1) != MaxLineBytes {
		t.Fatalf("oversized line len = %d; want cap %d", len(l1), MaxLineBytes)
	}
	if strings.Trim(l1, "A") != "" {
		t.Fatalf("oversized line should be all 'A' (truncated prefix), got stray bytes")
	}
	if !strings.Contains(out.String(), "too long") {
		t.Fatalf("user was not warned about truncation; out=%q", out.String())
	}

	l2, err := c.ReadLine()
	if err != nil {
		t.Fatal(err)
	}
	if l2 != "next" {
		t.Fatalf("line after overflow = %q; want %q (clean next line, drained to LF)", l2, "next")
	}
}

// TestReadLineCapDrainsNegotiation: IAC sequences inside the drained tail of an
// over-long line are still consumed and answered, so the cap never corrupts
// negotiation framing.
func TestReadLineCapDrainsNegotiation(t *testing.T) {
	var out bytes.Buffer
	var input []byte
	input = append(input, []byte(strings.Repeat("A", MaxLineBytes+10))...)
	input = append(input, iac, will, 1) // negotiation in the drained tail
	input = append(input, []byte(strings.Repeat("A", 10))...)
	input = append(input, '\n', 'o', 'k', '\n')
	c := NewReadWriter(bytes.NewReader(input), &out)

	if _, err := c.ReadLine(); err != nil {
		t.Fatal(err)
	}
	if l2, err := c.ReadLine(); err != nil || l2 != "ok" {
		t.Fatalf("line after overflow = %q, %v; want \"ok\"", l2, err)
	}
	if !bytes.Contains(out.Bytes(), []byte{iac, dont, 1}) {
		t.Fatalf("WILL in drained tail was not refused; out=% x", out.Bytes())
	}
}

// TestWriteStripsControlPreservesCRLF: outbound control runes are stripped
// (defense-in-depth) while CR/LF framing and multibyte runes survive, and 0xFF is
// still IAC-escaped.
func TestWriteStripsControlPreservesCRLF(t *testing.T) {
	var out bytes.Buffer
	c := NewReadWriter(&bytes.Buffer{}, &out)
	if err := c.Write("hi\x1b[31m\r\n😀\xff"); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if strings.ContainsRune(got, 0x1b) {
		t.Fatalf("output still contains ESC: %q", got)
	}
	if !strings.Contains(got, "\r\n") {
		t.Fatalf("output lost CR/LF framing: %q", got)
	}
	if !strings.Contains(got, "😀") {
		t.Fatalf("output lost multibyte rune: %q", got)
	}
	if !strings.Contains(got, "\xff\xff") {
		t.Fatalf("output did not IAC-escape 0xFF: % x", out.Bytes())
	}
}
