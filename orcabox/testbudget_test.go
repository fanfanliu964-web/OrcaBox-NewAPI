package orcabox

import (
	"errors"
	"testing"
)

func TestRealTestBudgetSurvivesFailedRequestAndRestart(t *testing.T) {
	t.Setenv("ORCABOX_REAL_TEST_BUDGET_UNITS", "1000")
	l := testLedger(t)
	l.Grant("budget-order", "budget-buyer", 100, 30)
	r, e := l.Reserve("budget-buyer", "m", "v1", 700)
	if e != nil {
		t.Fatal(e)
	}
	if e := l.Settle(r.ID, nil, Rate{}, true); e != nil {
		t.Fatal(e)
	}
	resumed := &Ledger{DB: l.DB}
	if _, e := resumed.Reserve("budget-buyer", "m", "v1", 301); !errors.Is(e, ErrTestBudget) {
		t.Fatal("released or restarted request restored test budget", e)
	}
	a, _ := l.Account("budget-buyer")
	if a.Available != 100*Unit || a.Frozen != 0 {
		t.Fatal("budget rejection modified customer balance")
	}
	if _, e := resumed.Reserve("budget-buyer", "m", "v1", 300); e != nil {
		t.Fatal(e)
	}
}

func TestTestBudgetIncludesPlatformOverrunOnceAcrossMigration(t *testing.T) {
	t.Setenv("ORCABOX_REAL_TEST_BUDGET_UNITS", "1000")
	l := testLedger(t)
	l.Grant("overrun-order", "buyer", 100, 30)
	rate := Rate{Input: "10", Output: "50", CacheRead: "0.5", CacheWrite: "12.5"}
	r, e := l.Reserve("buyer", "m", "v1", 700, rate)
	if e != nil {
		t.Fatal(e)
	}
	if e := l.Settle(r.ID, &Usage{Input: 80}, rate, false); e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 2; i++ {
		if e := l.Migrate(); e != nil {
			t.Fatal(e)
		}
	}
	var b TestBudget
	l.DB.First(&b, 1)
	if b.CommittedUnits != 800 {
		t.Fatal("platform expense omitted or counted twice", b.CommittedUnits)
	}
	if _, e := l.Reserve("buyer", "m2", "v1", 201); !errors.Is(e, ErrTestBudget) {
		t.Fatal("platform overrun did not consume spending budget")
	}
	a, _ := l.Account("buyer")
	if a.Used != 700 || a.Frozen != 0 {
		t.Fatal("platform overrun charged as customer debt")
	}
}
