package orcabox

import (
	"testing"
	"time"
)

func TestLegacyGrantEpochBackfillPreservesFundsAndRefundDuration(t *testing.T) {
	l := testLedger(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	l.Now = func() time.Time { return now }
	l.Grant("legacy-first", "buyer", 100, 30)
	now = now.Add(10 * 24 * time.Hour)
	l.Grant("legacy-renewal", "buyer", 100, 30)
	now = now.Add(70 * 24 * time.Hour)
	before, e := l.Grant("legacy-new-cycle", "buyer", 100, 30)
	if e != nil {
		t.Fatal(e)
	}
	if e := l.DB.Model(&Grant{}).Where("external_id = ?", "buyer").Update("epoch", 0).Error; e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 2; i++ {
		if e := l.Migrate(); e != nil {
			t.Fatal(e)
		}
	}
	a, _ := l.Account("buyer")
	if a.Available != before.Available || a.Expired != before.Expired || !a.ExpiresAt.Equal(before.ExpiresAt) {
		t.Fatal("metadata migration changed paid rights")
	}
	for order, expected := range map[string]int64{"legacy-first": 1, "legacy-renewal": 1, "legacy-new-cycle": 2} {
		var g Grant
		l.DB.First(&g, "order_no = ?", order)
		if g.Epoch != expected {
			t.Fatal("wrong reconstructed epoch", order, g.Epoch)
		}
	}
	var audits int64
	l.DB.Model(&Audit{}).Where("action = ?", "migration.grant_epoch_backfill").Count(&audits)
	if audits != 1 {
		t.Fatal("migration replay duplicated audit")
	}
	if e := l.Revoke("legacy-first", "buyer"); e != nil {
		t.Fatal(e)
	}
	a, _ = l.Account("buyer")
	if a.Available != before.Available || !a.ExpiresAt.Equal(before.ExpiresAt) {
		t.Fatal("old-cycle refund removed new-cycle rights")
	}
}

func TestRefundRemovesOnlyOnePurchasedDurationOnce(t *testing.T) {
	l := testLedger(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	l.Now = func() time.Time { return now }
	l.Grant("duration-first", "buyer", 100, 30)
	l.Grant("duration-renewal", "buyer", 100, 30)
	for i := 0; i < 100; i++ {
		if e := l.Revoke("duration-renewal", "buyer"); e != nil {
			t.Fatal(e)
		}
	}
	a, _ := l.Account("buyer")
	if a.Available != 100*Unit || !a.ExpiresAt.Equal(now.Add(30*24*time.Hour)) {
		t.Fatal("refund duration or credits repeated")
	}
}
