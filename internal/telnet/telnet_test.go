package telnet

import (
	"bytes"
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
