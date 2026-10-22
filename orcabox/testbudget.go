package orcabox

import (
	"encoding/json"
	"errors"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"os"
	"strconv"
)

var ErrTestBudget = errors.New("real-test budget exhausted")

type TestBudget struct {
	ID             int `gorm:"primaryKey"`
	CommittedUnits int64
}

func (TestBudget) TableName() string { return "orcabox_test_budgets" }

type TestBudgetCorrection struct {
	RequestID  string `gorm:"primaryKey;size:64"`
	ExtraUnits int64
}

func (TestBudgetCorrection) TableName() string { return "orcabox_test_budget_corrections" }

// Include the platform's extra cost even when the customer charge is capped.
func correctTestBudget(tx *gorm.DB, r Request, actual int64) error {
	limit, e := testBudgetLimit()
	if e != nil {
		return e
	}
	if limit == 0 || actual <= r.Reserved {
		return nil
	}
	row := TestBudgetCorrection{RequestID: r.ID, ExtraUnits: actual - r.Reserved}
	created := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
	if created.Error != nil {
		return created.Error
	}
	if created.RowsAffected == 0 {
		return nil
	}
	return tx.Model(&TestBudget{}).Where("id = 1").Update("committed_units", gorm.Expr("committed_units + ?", row.ExtraUnits)).Error
}
func (l *Ledger) repairTestBudget() error {
	limit, e := testBudgetLimit()
	if e != nil {
		return e
	}
	if limit == 0 {
		return nil
	}
	var rows []Request
	if e := l.DB.Where("status = ?", "overrun").Find(&rows).Error; e != nil {
		return e
	}
	for _, r := range rows {
		var rate Rate
		if json.Unmarshal([]byte(r.RateSnapshot), &rate) != nil {
			return ErrConflict
		}
		actual, e := rate.Cost(Usage{Input: r.Input, Output: r.Output, Cached: r.Cached, CacheWrite: r.CacheWrite})
		if e != nil {
			return e
		}
		if e := l.DB.Transaction(func(tx *gorm.DB) error { return correctTestBudget(tx, r, actual) }); e != nil {
			return e
		}
	}
	return nil
}
func testBudgetLimit() (int64, error) {
	raw := os.Getenv("ORCABOX_REAL_TEST_BUDGET_UNITS")
	if raw == "" {
		return 0, nil
	}
	n, e := strconv.ParseInt(raw, 10, 64)
	if e != nil || n < 1 || n > 2000000 {
		return 0, errors.New("real test budget must be between 1 and 2000000 units")
	}
	return n, nil
}

// Commit the worst-case cost forever, including failures and missing usage.
// Releasing a user's reservation never restores the real-test spending budget.
func commitTestBudget(tx *gorm.DB, units int64) error {
	limit, e := testBudgetLimit()
	if e != nil {
		return e
	}
	if limit == 0 {
		return nil
	}
	row := TestBudget{ID: 1}
	if e := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; e != nil {
		return e
	}
	result := tx.Model(&TestBudget{}).Where("id = 1 AND committed_units <= ?", limit-units).Update("committed_units", gorm.Expr("committed_units + ?", units))
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrTestBudget
	}
	return nil
}
