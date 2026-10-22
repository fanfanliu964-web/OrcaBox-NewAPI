package orcabox

import (
	"encoding/json"
	"errors"
	"github.com/shopspring/decimal"
	"math"
	"os"
	"time"

	"gorm.io/gorm/clause"
)

type Usage struct {
	Input      int64 `json:"input_tokens"`
	Output     int64 `json:"output_tokens"`
	Cached     int64 `json:"cached_tokens"`
	CacheWrite int64 `json:"cache_write_tokens"`
}
type Rate struct {
	Input      string `json:"input"`
	Output     string `json:"output"`
	CacheRead  string `json:"cache_read"`
	CacheWrite string `json:"cache_write"`
}
type PriceBook struct {
	Version       string          `json:"version"`
	Models        map[string]Rate `json:"models"`
	InputOverhead int64           `json:"supplier_input_reserve_tokens,omitempty"`
}

// Immutable published books make historical usage reproducible after a restart
// or price change. A version can never silently acquire different prices.
type PublishedPrices struct {
	Version   string `gorm:"primaryKey;size:100"`
	Snapshot  string `gorm:"type:text;not null"`
	CreatedAt time.Time
}

func (PublishedPrices) TableName() string { return "orcabox_published_prices" }
func (l *Ledger) PublishPrices(p PriceBook) error {
	b, e := json.Marshal(p)
	if e != nil {
		return e
	}
	row := PublishedPrices{Version: p.Version, Snapshot: string(b), CreatedAt: l.now()}
	if e = l.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; e != nil {
		return e
	}
	var saved PublishedPrices
	if e = l.DB.First(&saved, "version = ?", p.Version).Error; e != nil {
		return e
	}
	if saved.Snapshot != string(b) {
		return errors.New("published price version is immutable")
	}
	return nil
}

func LoadPrices() (PriceBook, error) {
	var p PriceBook
	b, e := os.ReadFile(os.Getenv("ORCABOX_PRICEBOOK"))
	if e != nil {
		return p, e
	}
	e = json.Unmarshal(b, &p)
	if e != nil || p.Version == "" || len(p.Models) == 0 || p.InputOverhead < 0 || p.InputOverhead > 32768 {
		return p, errors.New("invalid price book")
	}
	for _, r := range p.Models {
		for _, s := range []string{r.Input, r.Output, r.CacheRead, r.CacheWrite} {
			v, e := decimal.NewFromString(s)
			if e != nil || v.IsNegative() {
				return p, errors.New("invalid rate")
			}
		}
	}
	return p, nil
}

// Rates are credits per million tokens and the ledger has one million units per credit.
func (r Rate) Cost(u Usage) (int64, error) {
	if u.Input < 0 || u.Output < 0 || u.Cached < 0 || u.CacheWrite < 0 || u.Cached+u.CacheWrite > u.Input {
		return 0, errors.New("invalid usage")
	}
	total := decimal.Zero
	for _, part := range []struct {
		n     int64
		price string
	}{{u.Input - u.Cached - u.CacheWrite, r.Input}, {u.Output, r.Output}, {u.Cached, r.CacheRead}, {u.CacheWrite, r.CacheWrite}} {
		p, e := decimal.NewFromString(part.price)
		if e != nil || p.IsNegative() {
			return 0, errors.New("invalid rate")
		}
		total = total.Add(p.Mul(decimal.NewFromInt(part.n)))
	}
	if total.GreaterThan(decimal.NewFromInt(math.MaxInt64)) {
		return 0, errors.New("charge overflow")
	}
	return total.Ceil().IntPart(), nil
}
