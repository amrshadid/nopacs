package backup

import (
	"io"
	"sort"

	"github.com/amrshadid/nopacs/internal/secret"
	"github.com/amrshadid/nopacs/internal/snapshot"
)

// Verified is what reading a whole backup found, before anything is
// written.
type Verified struct {
	// Header is the file's header.
	Header Header
	// Starts and Ends are each section's first and last lines, in order.
	Starts []SectionStart
	Ends   []SectionEnd
	// Sums are each section's counts and digest, as the footer holds them.
	Sums []FooterSection
	// Lost is, per tenant in order, what the backup holds as not finished
	// at its recovery point and the restore cannot bring back.
	Lost []TenantLoss
}

// TenantLoss is what a restore cannot bring back of one tenant: the files
// that were only on the server's disk at the recovery point.
type TenantLoss struct {
	// Tenant is the tenant's name.
	Tenant string
	// Receptions are, per receiver in order of name, the receptions not
	// ended at the recovery point: their files acknowledged to the sender
	// and not yet archived were in incoming, and are gone. A sender must
	// send them again.
	Receptions []ReceptionLoss
	// QuarantineFiles and QuarantineBytes count the quarantined files held
	// at the recovery point: their records come back, their bytes, which
	// lived in incoming, do not.
	QuarantineFiles, QuarantineBytes int64
}

// ReceptionLoss is one receiver's receptions not ended.
type ReceptionLoss struct {
	// Receiver is its name.
	Receiver string
	// Receptions counts them; Files and Bytes what they had received and
	// not yet archived, held as duplicate or quarantined.
	Receptions, Files, Bytes int64
}

// Verify reads the whole backup from r with keys, authenticating every
// chunk, checking the layout, every count and every section's SHA-256
// against the footer, and the end of the file; it writes nothing. A file
// that fails anywhere is refused with an error.
func Verify(r io.Reader, keys *secret.Keys) (Verified, error) {
	br, err := secret.NewBackupReader(r, keys)
	if err != nil {
		return Verified{}, err
	}
	rd := newReader(br)
	var v Verified
	var loss *lossCounter
	rd.rowHook = func(table string, cols []snapshot.Column, values []any) {
		if loss != nil {
			loss.row(table, cols, values)
		}
	}
	if v.Header, err = rd.readHeader(); err != nil {
		return Verified{}, err
	}
	for {
		start, ok, err := rd.nextSection()
		if err != nil {
			return Verified{}, err
		}
		if !ok {
			break
		}
		loss = nil
		if start.Database == DatabaseTenant {
			loss = &lossCounter{byReceiver: map[string]*ReceptionLoss{}}
		}
		end, sum, err := rd.endSection()
		if err != nil {
			return Verified{}, err
		}
		v.Starts, v.Ends, v.Sums = append(v.Starts, start), append(v.Ends, end), append(v.Sums, sum)
		if loss != nil {
			v.Lost = append(v.Lost, loss.result(start.Tenant))
		}
	}
	return v, nil
}

// lossCounter counts one tenant's loss from the rows of its section.
type lossCounter struct {
	byReceiver           map[string]*ReceptionLoss
	quarantine, qBytes   int64
	receptionCols, qCols map[string]int
}

// index returns the position of each column by name.
func index(cols []snapshot.Column) map[string]int {
	out := make(map[string]int, len(cols))
	for i, c := range cols {
		out[c.Name] = i
	}
	return out
}

// intOf returns a value as an integer: an int as it is, a bool as 0 or 1.
func intOf(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case bool:
		if x {
			return 1
		}
	}
	return 0
}

// row counts one row of a table that says what is lost.
func (l *lossCounter) row(table string, cols []snapshot.Column, values []any) {
	switch table {
	case "receptions":
		if l.receptionCols == nil {
			l.receptionCols = index(cols)
		}
		c := l.receptionCols
		at := func(name string) any {
			if i, ok := c[name]; ok {
				return values[i]
			}
			return nil
		}
		if intOf(at("ended")) != 0 {
			return
		}
		receiver, _ := at("receiver").(string)
		r := l.byReceiver[receiver]
		if r == nil {
			r = &ReceptionLoss{Receiver: receiver}
			l.byReceiver[receiver] = r
		}
		r.Receptions++
		r.Files += max(0, intOf(at("files_received"))-intOf(at("files_archived"))-intOf(at("files_duplicate"))-intOf(at("files_quarantined")))
		r.Bytes += max(0, intOf(at("bytes_received"))-intOf(at("bytes_archived"))-intOf(at("bytes_duplicate"))-intOf(at("bytes_quarantined")))
	case "quarantine":
		if l.qCols == nil {
			l.qCols = index(cols)
		}
		si, ok1 := l.qCols["state"]
		zi, ok2 := l.qCols["size"]
		if !ok1 || !ok2 {
			return
		}
		if s, _ := values[si].(string); s == "held" {
			l.quarantine++
			l.qBytes += intOf(values[zi])
		}
	}
}

// result returns the loss counted.
func (l *lossCounter) result(tenantName string) TenantLoss {
	out := TenantLoss{Tenant: tenantName, QuarantineFiles: l.quarantine, QuarantineBytes: l.qBytes}
	for _, r := range l.byReceiver {
		out.Receptions = append(out.Receptions, *r)
	}
	sort.Slice(out.Receptions, func(i, j int) bool { return out.Receptions[i].Receiver < out.Receptions[j].Receiver })
	return out
}
