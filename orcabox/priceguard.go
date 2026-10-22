package orcabox

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/QuantumNous/new-api/common"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const supplierPricesURL = "https://tokenflux.dev/api/v1/marketplace/models?include_capacity=false"

type ModelGuard struct {
	Model        string `gorm:"primaryKey;size:100"`
	PriceVersion string
	Matches      bool
	CheckedAt    time.Time
	Locked       bool
	Reason       string `gorm:"size:50"`
	CauseVersion string `gorm:"size:100"`
}

func (l *Ledger) reopenModel(p PriceBook, modelName, reference string) error {
	if len(reference) < 12 || len(reference) > 160 {
		return ErrConflict
	}
	if _, ok := p.Models[modelName]; !ok {
		return ErrConflict
	}
	return l.DB.Transaction(func(tx *gorm.DB) error {
		var row ModelGuard
		if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, "model = ?", modelName).Error; e != nil {
			return e
		}
		if !row.Locked {
			return nil
		}
		if row.Reason != "reservation_overrun" || !row.Matches || row.PriceVersion != p.Version || row.CauseVersion == p.Version || p.InputOverhead < 8192 || l.now().Sub(row.CheckedAt) > 15*time.Minute {
			return ErrConflict
		}
		if e := tx.Create(&Audit{ExternalID: "operator:deployment", Action: "model.reopen:" + modelName + ":" + reference, CreatedAt: l.now()}).Error; e != nil {
			return e
		}
		return tx.Model(&row).Update("locked", false).Error
	})
}

func (ModelGuard) TableName() string { return "orcabox_model_guards" }
func requirePriceCheck() bool        { return os.Getenv("ORCABOX_REQUIRE_PRICE_CHECK") == "true" }
func (l *Ledger) modelReady(model, version string) bool {
	var row ModelGuard
	e := l.DB.First(&row, "model = ?", model).Error
	if !requirePriceCheck() {
		return e != nil || !row.Locked
	}
	return e == nil && !row.Locked && row.Matches && row.PriceVersion == version && l.now().Sub(row.CheckedAt) < 15*time.Minute
}

type supplierCatalog struct {
	Data []struct {
		Name   string
		Models []struct {
			ID      string
			Pricing struct {
				ContextIntervals []struct {
					MinTokens  int64           `json:"min_tokens"`
					MaxTokens  int64           `json:"max_tokens"`
					Input      json.RawMessage `json:"input_price_per_token"`
					Output     json.RawMessage `json:"output_price_per_token"`
					CacheRead  json.RawMessage `json:"cache_read_price_per_token"`
					CacheWrite json.RawMessage `json:"cache_write_price_per_token"`
				} `json:"context_intervals"`
			}
		}
	}
}

func catalogMatches(book PriceBook, raw []byte) (map[string]bool, error) {
	var catalog supplierCatalog
	if json.Unmarshal(raw, &catalog) != nil {
		return nil, errors.New("invalid supplier price catalog")
	}
	result := map[string]bool{}
	for model := range book.Models {
		result[model] = false
	}
	for _, group := range catalog.Data {
		if group.Name != "ChatGPT" {
			continue
		}
		for _, model := range group.Models {
			rate, ok := book.Models[model.ID]
			if !ok {
				continue
			}
			for _, tier := range model.Pricing.ContextIntervals {
				if tier.MinTokens != 0 || tier.MaxTokens < 128000+8192 {
					continue
				}
				matches := true
				for _, field := range []struct {
					raw      json.RawMessage
					approved string
				}{{tier.Input, rate.Input}, {tier.Output, rate.Output}, {tier.CacheRead, rate.CacheRead}, {tier.CacheWrite, rate.CacheWrite}} {
					remote, e := decimal.NewFromString(strings.Trim(string(field.raw), "\""))
					approved, err := decimal.NewFromString(field.approved)
					if e != nil || err != nil || remote.IsNegative() || remote.Mul(decimal.NewFromInt(1000000)).Sub(approved).Abs().GreaterThan(decimal.RequireFromString("0.00000001")) {
						matches = false
					}
				}
				result[model.ID] = matches
				break
			}
		}
	}
	return result, nil
}
func (l *Ledger) refreshPrices(ctx context.Context, p PriceBook) error {
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, supplierPricesURL, nil)
	if e != nil {
		return e
	}
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, e := client.Do(req)
	if e != nil {
		return errors.New("supplier price fetch unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return errors.New("supplier price fetch unavailable")
	}
	raw, e := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if e != nil || len(raw) > 1<<20 {
		return errors.New("supplier price catalog too large")
	}
	matches, e := catalogMatches(p, raw)
	if e != nil {
		return e
	}
	for model, match := range matches {
		row := ModelGuard{Model: model, PriceVersion: p.Version, CheckedAt: l.now(), Matches: match}
		if e := l.DB.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "model"}}, DoUpdates: clause.AssignmentColumns([]string{"price_version", "checked_at", "matches"})}).Create(&row).Error; e != nil {
			return e
		}
		if !match {
			common.SysLog("orcabox supplier price mismatch; model paused: " + model)
		}
	}
	return nil
}
func (l *Ledger) monitorPrices(p PriceBook) {
	if !requirePriceCheck() {
		return
	}
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		e := l.refreshPrices(ctx, p)
		cancel()
		if e != nil {
			common.SysLog("orcabox supplier price check pending; stale models stop accepting requests")
		}
		time.Sleep(5 * time.Minute)
	}
}
