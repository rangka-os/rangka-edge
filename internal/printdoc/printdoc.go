// Package printdoc lays out the documents an outlet prints on a thermal printer: the cashier receipt (struk) and the
// kitchen ticket (tiket dapur). A layout reads the record as the kernel returns it (GET /api/v1/r/pos_sale/<name>,
// module pos in rangka-apps) and writes ESC/POS through package escpos. Layouts are deterministic: the same record
// prints the same bytes, so a reprint after a paper jam is the same receipt.
//
// The receipt follows the 80 mm HTML print format pos_receipt of module pos (same order, same words), so a sale reads
// the same whether it was printed from the browser or through Edge.
package printdoc

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/rangka-os/rangka-edge/internal/escpos"
)

// Header is the business block at the top of a receipt (the site's letterhead: legal name, address, phone, NPWP).
type Header struct {
	Name    string `json:"name"`
	Address string `json:"address"`
	Phone   string `json:"phone"`
	NPWP    string `json:"npwp"`
	Footer  string `json:"footer"`
}

// Job is what `rangka-edge print` reads: the record plus what the layout needs around it. A bare pos_sale record (no
// "doc" key) is read as {"doc": record}.
type Job struct {
	Layout     string         `json:"layout"`      // receipt | kitchen
	Doc        map[string]any `json:"doc"`         // the pos_sale record
	Header     Header         `json:"header"`      // receipt only
	OutletName string         `json:"outlet_name"` // shown under the header (receipt) or at the top (kitchen)
	Station    string         `json:"station"`     // kitchen: the station the ticket is for (Dapur, Bar)
	PrintedAt  string         `json:"printed_at"`  // RFC3339; kitchen prints its time, receipt nothing extra
	Drawer     bool           `json:"drawer"`      // receipt: kick the cash drawer (a cash payment)
	Copy       bool           `json:"copy"`        // a reprint: prints SALINAN
}

// ParseJob reads a job; a JSON object without "doc" is taken as the record itself.
func ParseJob(raw []byte) (Job, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil || probe == nil {
		return Job{}, fmt.Errorf("print job must be a JSON object")
	}
	var j Job
	if _, ok := probe["doc"]; ok {
		if err := json.Unmarshal(raw, &j); err != nil {
			return Job{}, fmt.Errorf("print job: %v", err)
		}
	} else {
		var rec map[string]any
		if err := json.Unmarshal(raw, &rec); err != nil {
			return Job{}, err
		}
		if r, ok := rec["record"].(map[string]any); ok { // the kernel's {record: ...} envelope
			rec = r
		}
		j.Doc = rec
	}
	if j.Doc == nil || str(j.Doc["name"]) == "" {
		return Job{}, fmt.Errorf("print job has no record name")
	}
	return j, nil
}

// Render lays out the job for a printer with cols columns.
func Render(j Job, cols int) ([]byte, error) {
	switch j.Layout {
	case "", "receipt":
		return Receipt(j, cols), nil
	case "kitchen":
		return Kitchen(j, cols), nil
	}
	return nil, fmt.Errorf("unknown layout %q (receipt, kitchen)", j.Layout)
}

// Receipt is the cashier receipt of a pos_sale.
func Receipt(j Job, cols int) []byte {
	d := j.Doc
	b := escpos.New(cols)
	b.Align(escpos.Center)
	if j.Header.Name != "" {
		b.Bold(true).Size(true, false).Line(j.Header.Name).Size(false, false).Bold(false)
	}
	for _, l := range strings.Split(j.Header.Address, "\n") {
		if strings.TrimSpace(l) != "" {
			b.Line(strings.TrimSpace(l))
		}
	}
	if j.Header.Phone != "" {
		b.Line(j.Header.Phone)
	}
	if j.Header.NPWP != "" {
		b.Line("NPWP " + j.Header.NPWP)
	}
	if j.OutletName != "" {
		b.Line(j.OutletName)
	}
	if j.Copy {
		b.Bold(true).Line("SALINAN").Bold(false)
	}
	b.Align(escpos.Left).Rule('-')
	b.Pair("Struk", str(d["name"]))
	b.Pair("Tanggal", Date(str(d["date"])))
	if v := str(d["buyer"]); v != "" {
		b.Pair("Pembeli", v)
	}
	b.Rule('-')
	for _, row := range rows(d["items"]) {
		name := str(row["description"])
		if name == "" {
			name = str(row["item"])
		}
		b.Line(name)
		left := Number(num(row["qty"])) + " x " + Rupiah(num(row["rate"]))
		if p := num(row["discount_pct"]); p != 0 {
			left += " (-" + Number(p) + "%)"
		}
		b.Pair("  "+left, Rupiah(num(row["amount"])))
		if n := str(row["note"]); n != "" {
			b.Line("  " + n)
		}
	}
	b.Rule('-')
	if v := num(d["discount_amount"]); v != 0 {
		b.Pair("Subtotal", Rupiah(num(d["subtotal"])))
		b.Pair("Diskon nota", "-"+Rupiah(v))
	}
	b.Bold(true).Pair("Total", Rupiah(num(d["grand_total"]))).Bold(false)
	if v := num(d["ppn_in_sales"]); v != 0 {
		b.Pair("Termasuk PPN", Rupiah(v))
	}
	pays := rows(d["payments"])
	for _, p := range pays {
		label := "Tunai"
		if str(p["method"]) == "qris" {
			label = "QRIS"
			if r := str(p["ref"]); r != "" {
				label += " " + r
			}
		}
		b.Pair(label, Rupiah(num(p["amount"])))
	}
	if len(pays) == 0 {
		b.Pair("Tunai", Rupiah(num(d["grand_total"])))
	}
	if v := num(d["change_amount"]); v != 0 {
		b.Pair("Kembalian", Rupiah(v))
	}
	b.Align(escpos.Center).Bold(true)
	switch str(d["docstatus"]) {
	case "0":
		b.Line("DRAF, BELUM DIBAYAR")
	case "2":
		b.Line("BATAL")
	default:
		b.Line("LUNAS")
	}
	b.Bold(false).Rule('-')
	footer := j.Header.Footer
	if footer == "" {
		footer = "Terima kasih"
	}
	b.Line(footer)
	b.Feed(3).Cut()
	if j.Drawer {
		b.OpenDrawer()
	}
	return b.Bytes()
}

// Kitchen is the kitchen ticket of a pos_sale: what to make, big enough to read from the stove, no prices.
func Kitchen(j Job, cols int) []byte {
	d := j.Doc
	b := escpos.New(cols)
	b.Align(escpos.Center).Bold(true).Size(true, true)
	station := j.Station
	if station == "" {
		station = "DAPUR"
	}
	b.Line(strings.ToUpper(station))
	b.Size(false, false).Bold(false)
	if j.OutletName != "" {
		b.Line(j.OutletName)
	}
	if j.Copy {
		b.Bold(true).Line("SALINAN").Bold(false)
	}
	b.Align(escpos.Left).Rule('=')
	b.Pair("Pesanan", str(d["name"]))
	if v := str(d["buyer"]); v != "" {
		b.Pair("Atas nama", v)
	}
	if t, err := time.Parse(time.RFC3339, j.PrintedAt); err == nil {
		b.Pair("Jam", t.Format("15:04"))
	}
	b.Rule('=')
	for _, row := range rows(d["items"]) {
		name := str(row["description"])
		if name == "" {
			name = str(row["item"])
		}
		b.Bold(true).Size(false, true).Line(Number(num(row["qty"]))+" x "+name).Size(false, false).Bold(false)
		if n := str(row["note"]); n != "" {
			b.Line("   * " + n)
		}
	}
	b.Rule('=')
	b.Feed(3).Cut()
	return b.Bytes()
}

func rows(v any) []map[string]any {
	list, _ := v.([]any)
	out := make([]map[string]any, 0, len(list))
	for _, x := range list {
		if m, ok := x.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func str(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	case json.Number:
		return x.String()
	}
	return fmt.Sprint(v)
}

func num(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f
	case json.Number:
		f, _ := x.Float64()
		return f
	}
	return 0
}

// Rupiah formats a whole-rupiah amount the way the kernel's print helper does: "Rp 1.250.000", "-Rp 5.000"
// (rounded half away from zero).
func Rupiah(n float64) string {
	r := math.Round(n)
	if r < 0 {
		return "-Rp " + thousands(int64(-r))
	}
	return "Rp " + thousands(int64(r))
}

// Number formats a quantity with Indonesian separators, dropping trailing zeros: 2 -> "2", 1.5 -> "1,5".
func Number(n float64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := strconv.FormatFloat(n, 'f', 3, 64)
	whole, frac, _ := strings.Cut(s, ".")
	frac = strings.TrimRight(frac, "0")
	w, _ := strconv.ParseInt(whole, 10, 64)
	out := thousands(w)
	if frac != "" {
		out += "," + frac
	}
	if neg {
		out = "-" + out
	}
	return out
}

func thousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, '.')
		}
		out = append(out, c)
	}
	return string(out)
}

var months = []string{"Jan", "Feb", "Mar", "Apr", "Mei", "Jun", "Jul", "Agu", "Sep", "Okt", "Nov", "Des"}

// Date prints YYYY-MM-DD as "27 Okt 2026" (the kernel's date helper); anything else as is.
func Date(s string) string {
	if len(s) < 10 {
		return s
	}
	t, err := time.Parse("2006-01-02", s[:10])
	if err != nil {
		return s
	}
	return fmt.Sprintf("%d %s %d", t.Day(), months[t.Month()-1], t.Year())
}
