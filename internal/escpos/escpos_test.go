package escpos

import (
	"bytes"
	"strings"
	"testing"
)

func TestBytesOfCommands(t *testing.T) {
	b := New(Cols80).Align(Center).Bold(true).Line("Tahu").Bold(false).Cut().OpenDrawer().Bytes()
	want := []byte{0x1b, '@', 0x1b, 'a', 1, 0x1b, 'E', 1, 'T', 'a', 'h', 'u', '\n', 0x1b, 'E', 0, 0x1d, 'V', 66, 3, 0x1b, 'p', 0, 25, 250}
	if !bytes.Equal(b, want) {
		t.Fatalf("bytes\n got %v\nwant %v", b, want)
	}
}

func TestPairFitsAndWraps(t *testing.T) {
	got := Decode(New(32).Pair("Total", "Rp 48.000").Bytes())
	if got != "Total                  Rp 48.000\n" || len(strings.TrimSuffix(got, "\n")) != 32 {
		t.Fatalf("pair: %q", got)
	}
	got = Decode(New(20).Pair("Tahu goreng original pedas", "Rp 30.000").Bytes())
	want := "Tahu goreng original\npedas\n           Rp 30.000\n"
	if got != want {
		t.Fatalf("wrapped pair:\n%q\nwant\n%q", got, want)
	}
}

func TestDoubleWidthHalvesColumns(t *testing.T) {
	b := New(Cols80).Size(true, false)
	if b.Cols() != 24 {
		t.Fatalf("cols %d", b.Cols())
	}
	got := Decode(b.Rule('-').Bytes())
	if got != strings.Repeat("-", 24)+"\n" {
		t.Fatalf("rule %q", got)
	}
}

func TestCleanFoldsToASCII(t *testing.T) {
	for in, want := range map[string]string{
		"Café Ñusa":      "Cafe Nusa",
		"Kopi\tsusu\n":   "Kopi susu ",
		"Es teh – manis": "Es teh - manis",
		"Nasi 🍚 goreng":  "Nasi ? goreng",
		"‘Spesial’ “ok”": "'Spesial' \"ok\"",
	} {
		if got := Clean(in); got != want {
			t.Errorf("Clean(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWrapLongWord(t *testing.T) {
	got := Wrap("ABCDEFGHIJKLMNOP", 6)
	if strings.Join(got, "|") != "ABCDEF|GHIJKL|MNOP" {
		t.Fatalf("%q", got)
	}
}

func TestQRRoundTrip(t *testing.T) {
	data := "00020101021226670016COM.CONTOH.WWW"
	got := Decode(New(Cols80).QR(data).Bytes())
	if !strings.Contains(got, "[QR "+data+"]") {
		t.Fatalf("qr not decoded: %q", got)
	}
	if out := New(Cols80).QR(strings.Repeat("x", 701)).Bytes(); len(out) != 2 {
		t.Fatalf("over-long QR printed %d bytes", len(out))
	}
}

func TestDecodeMarksUnknownCommands(t *testing.T) {
	if got := Decode([]byte{0x1b, 'Z', 'a'}); got != "[ESC 'Z']a" {
		t.Fatalf("%q", got)
	}
}
