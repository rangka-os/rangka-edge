package printdoc

import (
	"strings"
	"testing"

	"github.com/rangka-os/rangka-edge/internal/escpos"
)

const saleJSON = `{"doc": {"name": "STRK-DGO-2611-00007", "date": "2026-11-11", "docstatus": 1, "buyer": "Meja 4",
 "items": [{"item": "BRG-0007", "description": "Tahu goreng original", "qty": 2, "rate": 15000, "amount": 30000, "note": "tidak pedas"},
           {"item": "BRG-0009", "description": "Tahu isi sayur", "qty": 1.5, "rate": 20000, "discount_pct": 10, "amount": 27000}],
 "subtotal": 57000, "discount_amount": 2000, "grand_total": 55000, "ppn_in_sales": 0,
 "payments": [{"method": "cash", "amount": 40000}, {"method": "qris", "amount": 15000, "ref": "INV-7781"}], "change_amount": 0},
 "header": {"name": "Tahu Contoh", "address": "Jl. Contoh Dago No. 1\nBandung", "npwp": "contoh-npwp"},
 "outlet_name": "Outlet Dago", "printed_at": "2026-11-11T12:05:00+07:00"}`

func TestReceipt80mm(t *testing.T) {
	j, err := ParseJob([]byte(saleJSON))
	if err != nil {
		t.Fatal(err)
	}
	got := escpos.Decode(Receipt(j, escpos.Cols80))
	for _, want := range []string{
		"Tahu Contoh\n", "Jl. Contoh Dago No. 1\nBandung\n", "NPWP contoh-npwp\n", "Outlet Dago\n",
		"Struk                        STRK-DGO-2611-00007\n",
		"Tanggal                              11 Nov 2026\n",
		"Tahu goreng original\n  2 x Rp 15.000                        Rp 30.000\n  tidak pedas\n",
		"  1,5 x Rp 20.000 (-10%)               Rp 27.000\n",
		"Subtotal                               Rp 57.000\n", "Diskon nota                            -Rp 2.000\n",
		"Total                                  Rp 55.000\n", "Tunai                                  Rp 40.000\n",
		"QRIS INV-7781                          Rp 15.000\n", "LUNAS\n", "Terima kasih\n", "[POTONG]\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("receipt lacks %q\n%s", want, got)
		}
	}
	if strings.Contains(got, "[LACI]") || strings.Contains(got, "Kembalian") || strings.Contains(got, "SALINAN") {
		t.Errorf("unexpected drawer, change or copy:\n%s", got)
	}
	for _, l := range strings.Split(got, "\n") {
		if len(l) > escpos.Cols80 {
			t.Errorf("line over 48 columns: %q", l)
		}
	}
}

func TestReceiptSameBytesTwice(t *testing.T) {
	j, _ := ParseJob([]byte(saleJSON))
	if string(Receipt(j, 48)) != string(Receipt(j, 48)) {
		t.Fatal("a reprint must be the same bytes")
	}
}

func TestReceiptDraftCopyDrawer58mm(t *testing.T) {
	j, _ := ParseJob([]byte(`{"name":"DRAFT-1","docstatus":0,"items":[],"grand_total":12000,"change_amount":8000}`))
	j.Copy, j.Drawer = true, true
	got := escpos.Decode(Receipt(j, escpos.Cols58))
	for _, want := range []string{"SALINAN\n", "DRAF, BELUM DIBAYAR\n", "Tunai                  Rp 12.000\n", "Kembalian               Rp 8.000\n", "[POTONG]\n[LACI]\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("lacks %q\n%s", want, got)
		}
	}
}

func TestKitchenTicket(t *testing.T) {
	j, _ := ParseJob([]byte(saleJSON))
	j.Station = "bar"
	got := escpos.Decode(Kitchen(j, escpos.Cols80))
	for _, want := range []string{"BAR\n", "Outlet Dago\n", "Pesanan                      STRK-DGO-2611-00007\n", "Atas nama                                 Meja 4\n",
		"Jam                                        12:05\n", "2 x Tahu goreng original\n   * tidak pedas\n", "1,5 x Tahu isi sayur\n", "[POTONG]\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("ticket lacks %q\n%s", want, got)
		}
	}
	if strings.Contains(got, "Rp") {
		t.Errorf("a kitchen ticket carries no prices:\n%s", got)
	}
}

func TestParseJobEnvelopeAndErrors(t *testing.T) {
	j, err := ParseJob([]byte(`{"record": {"name": "S-1", "items": []}}`))
	if err != nil || j.Doc["name"] != "S-1" {
		t.Fatalf("envelope: %v %v", j, err)
	}
	if _, err := ParseJob([]byte(`[1]`)); err == nil {
		t.Fatal("array accepted")
	}
	if _, err := ParseJob([]byte(`{"doc": {"items": []}}`)); err == nil {
		t.Fatal("record without name accepted")
	}
	if _, err := Render(Job{Layout: "label", Doc: map[string]any{"name": "x"}}, 48); err == nil {
		t.Fatal("unknown layout accepted")
	}
}

func TestFormats(t *testing.T) {
	for n, want := range map[float64]string{0: "Rp 0", 1250000: "Rp 1.250.000", -5000: "-Rp 5.000", 999.5: "Rp 1.000"} {
		if got := Rupiah(n); got != want {
			t.Errorf("Rupiah(%v) = %q, want %q", n, got, want)
		}
	}
	for n, want := range map[float64]string{2: "2", 1.5: "1,5", 1000: "1.000", 0.25: "0,25", -3: "-3"} {
		if got := Number(n); got != want {
			t.Errorf("Number(%v) = %q, want %q", n, got, want)
		}
	}
	if Date("2026-10-27") != "27 Okt 2026" || Date("x") != "x" {
		t.Error("Date")
	}
}
