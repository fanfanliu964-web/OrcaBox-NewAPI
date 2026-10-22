package orcabox

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

type settlement struct {
	ID       string
	Usage    Usage
	Rate     Rate
	Metadata []SettlementMetadata
}

func spoolDir() string {
	if p := os.Getenv("ORCABOX_SETTLEMENT_DIR"); p != "" {
		return p
	}
	return "/data/orcabox-settlements"
}
func (l *Ledger) DurableSettle(id string, u Usage, r Rate, metadata ...SettlementMetadata) error {
	if len(id) != 64 || strings.ContainsAny(id, "/\\.") {
		return errors.New("invalid settlement ID")
	}
	dir := spoolDir()
	if e := os.MkdirAll(dir, 0700); e != nil {
		return e
	}
	b, e := json.Marshal(settlement{ID: id, Usage: u, Rate: r, Metadata: metadata})
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(dir, "pending-")
	if e != nil {
		return e
	}
	name := f.Name()
	defer os.Remove(name)
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(b)
	}
	if e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e == nil {
		e = closeErr
	}
	if e != nil {
		return e
	}
	dest := filepath.Join(dir, id+".json")
	if e = os.Rename(name, dest); e != nil {
		return e
	}
	if d, e := os.Open(dir); e == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	if e = l.Settle(id, &u, r, false, metadata...); e != nil {
		return e
	}
	return os.Remove(dest)
}
func (l *Ledger) recoverSpool() error {
	files, e := os.ReadDir(spoolDir())
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		return e
	}
	for _, f := range files {
		if !strings.HasSuffix(f.Name(), ".json") {
			continue
		}
		p := filepath.Join(spoolDir(), f.Name())
		b, e := os.ReadFile(p)
		if e != nil {
			return e
		}
		var s settlement
		if json.Unmarshal(b, &s) != nil || f.Name() != s.ID+".json" {
			return errors.New("invalid settlement spool")
		}
		if e = l.Settle(s.ID, &s.Usage, s.Rate, false, s.Metadata...); e != nil {
			return e
		}
		if e = os.Remove(p); e != nil {
			return e
		}
	}
	return nil
}
