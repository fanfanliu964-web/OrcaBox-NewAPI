package orcabox

import (
	"fmt"
	"testing"
	"time"
)

func TestSupplierPriceGateVersionStalenessAndIncrease(t *testing.T) {
	l := testLedger(t)
	t.Setenv("ORCABOX_REQUIRE_PRICE_CHECK", "true")
	p := PriceBook{Version: "published", Models: map[string]Rate{"m": {Input: "10", Output: "50", CacheRead: "0.5", CacheWrite: "12.5"}}}
	fixture := func(output string) []byte {
		return []byte(fmt.Sprintf(`{"data":[{"name":"ChatGPT","models":[{"id":"m","pricing":{"context_intervals":[{"min_tokens":0,"max_tokens":200000,"input_price_per_token":0.00001,"output_price_per_token":%s,"cache_read_price_per_token":0.0000005,"cache_write_price_per_token":0.0000125}]}}]}]}`, output))
	}
	m, e := catalogMatches(p, fixture("0.00005"))
	if e != nil || !m["m"] {
		t.Fatal("equal final credit rates rejected", e)
	}
	m, e = catalogMatches(p, fixture("0.00006"))
	if e != nil || m["m"] {
		t.Fatal("supplier price increase accepted", e)
	}
	if l.modelReady("m", p.Version) {
		t.Fatal("unverified model open")
	}
	row := ModelGuard{Model: "m", Matches: true, PriceVersion: p.Version, CheckedAt: l.now()}
	l.DB.Create(&row)
	if !l.modelReady("m", p.Version) || l.modelReady("m", "different") {
		t.Fatal("version not enforced")
	}
	row.CheckedAt = l.now().Add(-16 * time.Minute)
	l.DB.Save(&row)
	if l.modelReady("m", p.Version) {
		t.Fatal("stale price check accepted")
	}
	row.CheckedAt = l.now()
	row.Locked = true
	l.DB.Save(&row)
	if l.modelReady("m", p.Version) {
		t.Fatal("overrun suspension cleared by price match")
	}
}

func TestModelReopenRequiresNewReviewedReservationVersion(t *testing.T) {
	l := testLedger(t)
	p := PriceBook{Version: "v2", InputOverhead: 8192, Models: map[string]Rate{"m": {Input: "10", Output: "50", CacheRead: "0.5", CacheWrite: "12.5"}}}
	row := ModelGuard{Model: "m", Matches: true, PriceVersion: "v2", CheckedAt: l.now(), Locked: true, Reason: "reservation_overrun", CauseVersion: "v1"}
	l.DB.Create(&row)
	old := p
	old.Version = "v1"
	if e := l.reopenModel(old, "m", "operator-reviewed-fixture"); e == nil {
		t.Fatal("old reservation rule reopened suspended model")
	}
	bad := p
	bad.InputOverhead = 0
	if e := l.reopenModel(bad, "m", "operator-reviewed-fixture"); e == nil {
		t.Fatal("unfixed overhead reopened model")
	}
	for i := 0; i < 100; i++ {
		if e := l.reopenModel(p, "m", "operator-reviewed-fixture"); e != nil {
			t.Fatal(e)
		}
	}
	var count int64
	l.DB.Model(&Audit{}).Where("action LIKE ?", "model.reopen:%").Count(&count)
	if count != 1 {
		t.Fatal("replayed operator action changed audit", count)
	}
}
