package orcabox

import (
	"bytes"
	"errors"
	"github.com/QuantumNous/new-api/model"
	"github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testLedger(t *testing.T) *Ledger {
	t.Helper()
	db, e := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if e != nil {
		t.Fatal(e)
	}
	sql, _ := db.DB()
	sql.SetMaxOpenConns(1)
	t.Cleanup(func() { sql.Close() })
	if e = db.AutoMigrate(&model.User{}, &model.Token{}); e != nil {
		t.Fatal(e)
	}
	l := &Ledger{DB: db}
	if e = l.Migrate(); e != nil {
		t.Fatal(e)
	}
	return l
}
func TestGrantReplayRotationAndExpiry(t *testing.T) {
	l := testLedger(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	l.Now = func() time.Time { return now }
	a, e := l.Grant("order-1", "buyer-1", 100, 30)
	if e != nil {
		t.Fatal(e)
	}
	key, _ := l.Key("buyer-1")
	for i := 0; i < 100; i++ {
		a, e = l.Grant("order-1", "buyer-1", 100, 30)
		if e != nil || a.Available != 100*Unit {
			t.Fatalf("replayed grant changed balance: %v", e)
		}
	}
	if _, e = l.Grant("order-1", "buyer-2", 100, 30); !errors.Is(e, ErrConflict) {
		t.Fatal("cross-account replay accepted")
	}
	if e = l.Rotate("buyer-1"); e != nil {
		t.Fatal(e)
	}
	rotated, _ := l.Key("buyer-1")
	if rotated == key {
		t.Fatal("key unchanged")
	}
	a, e = l.Grant("order-2", "buyer-1", 100, 30)
	if e != nil || a.Available != 200*Unit || !a.ExpiresAt.Equal(now.Add(60*24*time.Hour)) {
		t.Fatal("renewal incorrect", e)
	}
	r, e := l.Reserve("buyer-1", "m", "v1", 10*Unit)
	if e != nil {
		t.Fatal(e)
	}
	now = now.Add(61 * 24 * time.Hour)
	a, e = l.Grant("order-3", "buyer-1", 100, 30)
	if e != nil || a.Available != 100*Unit {
		t.Fatal("expired balance restored")
	}
	if e = l.Settle(r.ID, nil, Rate{}, true); e != nil {
		t.Fatal(e)
	}
	a, _ = l.Account("buyer-1")
	if a.Available != 100*Unit || a.Frozen != 0 {
		t.Fatal("old reservation resurrected expired credits")
	}
}
func TestReserveAndSettleAtMostOnce(t *testing.T) {
	l := testLedger(t)
	_, e := l.Grant("order-1", "buyer", 100, 30)
	if e != nil {
		t.Fatal(e)
	}
	r, e := l.Reserve("buyer", "m", "v1", 60*Unit)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = l.Reserve("buyer", "m", "v1", 60*Unit); !errors.Is(e, ErrFunds) {
		t.Fatal("overdraft accepted")
	}
	rate := Rate{Input: "10", Output: "50", CacheRead: "0.5", CacheWrite: "12.5"}
	u := Usage{Input: 1000000, Output: 100000, Cached: 200000, CacheWrite: 100000}
	expected, _ := rate.Cost(u)
	for i := 0; i < 3; i++ {
		if e = l.Settle(r.ID, &u, rate, false); e != nil {
			t.Fatal(e)
		}
	}
	a, _ := l.Account("buyer")
	if a.Available != 100*Unit-expected || a.Frozen != 0 || a.Used != expected {
		t.Fatalf("incorrect ledger %+v", a)
	}
}
func TestMissingUsageRecovery(t *testing.T) {
	l := testLedger(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	l.Now = func() time.Time { return now }
	l.Grant("order-1", "buyer", 100, 30)
	r, e := l.Reserve("buyer", "m", "v1", Unit)
	if e != nil {
		t.Fatal(e)
	}
	if e = l.Settle(r.ID, nil, Rate{}, false); e != nil {
		t.Fatal(e)
	}
	a, _ := l.Account("buyer")
	if a.Frozen != Unit {
		t.Fatal("uncertain request prematurely released")
	}
	now = now.Add(25 * time.Hour)
	if e = l.Recover(); e != nil {
		t.Fatal(e)
	}
	a, _ = l.Account("buyer")
	if a.Frozen != 0 || a.Available != 100*Unit || a.Used != 0 {
		t.Fatal("missing usage recovery incorrect")
	}
}
func TestRawCaptureDoesNotInventUsage(t *testing.T) {
	c := usageCapture{stream: true}
	for _, b := range bytes.Split([]byte("data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":20,\"output_tokens\":10,\"input_tokens_details\":{\"cached_tokens\":4}}}}\n\n"), []byte("")) {
		c.feed(b)
	}
	if c.usage == nil || c.usage.Input != 20 || c.usage.Cached != 4 {
		t.Fatal("SSE usage lost")
	}
	missing := usageCapture{stream: true}
	missing.feed([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"))
	if missing.usage != nil {
		t.Fatal("fabricated usage")
	}
}
func TestRequestGuards(t *testing.T) {
	for _, b := range []map[string]any{{"service_tier": "priority"}, {"previous_response_id": "resp-other-user"}, {"tools": []any{map[string]any{"type": "web_search"}}}, {"input": []any{map[string]any{"type": "input_image", "image_url": "http://169.254.169.254/"}}}, {"max_output_tokens": float64(99999)}} {
		if _, _, e := validateRequest(b, "/v1/responses"); e == nil {
			t.Fatal("unsafe request accepted")
		}
	}
	for _, b := range []map[string]any{{"n": float64(2)}, {"modalities": []any{"text", "audio"}}} {
		if _, _, e := validateRequest(b, "/v1/chat/completions"); e == nil {
			t.Fatal("multiple or audio outputs bypassed the cost ceiling")
		}
	}
	b := map[string]any{"max_tokens": float64(32), "max_completion_tokens": float64(8192)}
	_, limit, e := validateRequest(b, "/v1/chat/completions")
	if e != nil || limit != 32 || b["max_completion_tokens"] != int64(32) || b["max_tokens"] != nil {
		t.Fatal("conflicting output aliases escaped the reserved maximum")
	}
}

func TestPublishedPricesImmutableAndRequestSnapshot(t *testing.T) {
	l := testLedger(t)
	r := Rate{Input: "10", Output: "50", CacheRead: "0.5", CacheWrite: "12.5"}
	p := PriceBook{Version: "v1", Models: map[string]Rate{"m": r}}
	if e := l.PublishPrices(p); e != nil {
		t.Fatal(e)
	}
	if e := l.PublishPrices(p); e != nil {
		t.Fatal(e)
	}
	p.Models["m"] = Rate{Input: "11"}
	if e := l.PublishPrices(p); e == nil {
		t.Fatal("published version overwritten")
	}
	l.Grant("price-order", "price-buyer", 100, 30)
	q, e := l.Reserve("price-buyer", "m", "v1", Unit, r)
	if e != nil || q.RateSnapshot == "" {
		t.Fatal("request omitted its rate snapshot", e)
	}
}

func TestDurableUsageSurvivesDatabaseFailure(t *testing.T) {
	l := testLedger(t)
	dir := t.TempDir()
	t.Setenv("ORCABOX_SETTLEMENT_DIR", dir)
	l.Grant("spool-order", "spool-buyer", 100, 30)
	r, e := l.Reserve("spool-buyer", "m", "v1", Unit)
	if e != nil {
		t.Fatal(e)
	}
	brokenDB, e := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if e != nil {
		t.Fatal(e)
	}
	sql, _ := brokenDB.DB()
	sql.Close()
	broken := &Ledger{DB: brokenDB}
	rate := Rate{Input: "10", Output: "50", CacheRead: "0.5", CacheWrite: "12.5"}
	if e = broken.DurableSettle(r.ID, Usage{Input: 20, Output: 10, Cached: 4}, rate); e == nil {
		t.Fatal("closed database unexpectedly settled")
	}
	if _, e = os.Stat(filepath.Join(dir, r.ID+".json")); e != nil {
		t.Fatal("numeric usage not durably retained", e)
	}
	if e = l.Recover(); e != nil {
		t.Fatal(e)
	}
	if e = l.Recover(); e != nil {
		t.Fatal(e)
	}
	a, _ := l.Account("spool-buyer")
	if a.Frozen != 0 || a.Used != 662 || a.Available != 100*Unit-662 {
		t.Fatal("database recovery lost or duplicated the charge")
	}
	files, _ := os.ReadDir(dir)
	if len(files) != 0 {
		t.Fatal("settled spool not cleared")
	}
}

func TestPersistentConcurrencySurvivesLostRedisLeases(t *testing.T) {
	l := testLedger(t)
	l.Grant("concurrency-order", "concurrency-buyer", 100, 30)
	first, e := l.Reserve("concurrency-buyer", "m", "v1", Unit)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = l.Reserve("concurrency-buyer", "m", "v1", Unit); e != nil {
		t.Fatal(e)
	}
	if _, e = l.Reserve("concurrency-buyer", "m", "v1", Unit); !errors.Is(e, ErrConcurrency) {
		t.Fatal("third active reservation bypassed durable concurrency control")
	}
	if e = l.Settle(first.ID, nil, Rate{}, true); e != nil {
		t.Fatal(e)
	}
	if _, e = l.Reserve("concurrency-buyer", "m", "v1", Unit); e != nil {
		t.Fatal(e)
	}
}

func TestOpaqueReasoningReplayRequiresOwnedVerifiedUsage(t *testing.T) {
	l := testLedger(t)
	a, _ := l.Grant("opaque-order-a", "opaque-buyer-a", 100, 30)
	b, _ := l.Grant("opaque-order-b", "opaque-buyer-b", 100, 30)
	item := map[string]any{"type": "reasoning", "id": "rs_fixture", "encrypted_content": "synthetic opaque bytes", "summary": []any{}}
	body := map[string]any{"input": []any{item}}
	if _, _, e := validateRequest(body, "/v1/responses"); e == nil {
		t.Fatal("unverified opaque reasoning accepted")
	}
	r, e := l.Reserve(a.ExternalID, "m", "v1", Unit)
	if e != nil {
		t.Fatal(e)
	}
	rate := Rate{Input: "10", Output: "50", CacheRead: "0.5", CacheWrite: "12.5"}
	meta := SettlementMetadata{RequestID: "req_fixture", ResponseID: "resp_fixture", OpaqueDigests: []string{opaqueDigest(item)}}
	if e = l.Settle(r.ID, &Usage{Input: 20, Output: 10}, rate, false, meta); e != nil {
		t.Fatal(e)
	}
	if n, e := l.opaqueCeiling(a.ID, item); e != nil || n != 10 {
		t.Fatal("verified owner cannot replay reasoning", e)
	}
	if _, e := l.opaqueCeiling(b.ID, item); e == nil {
		t.Fatal("cross-user reasoning replay accepted")
	}
	// A second HTTP request is decoded into a fresh map; validation normalizes
	// its output-limit aliases in place.
	body = map[string]any{"input": []any{item}}
	if _, _, e := validateRequest(body, "/v1/responses", func(item map[string]any) (int64, error) { return l.opaqueCeiling(a.ID, item) }); e != nil {
		t.Fatal(e)
	}
	var saved Request
	l.DB.First(&saved, "id = ?", r.ID)
	if saved.SupplierRequestID != "req_fixture" || saved.SupplierResponseID != "resp_fixture" {
		t.Fatal("supplier reconciliation identifiers missing")
	}
	for _, unsafe := range []map[string]any{{"type": "item_reference", "id": "msg_hidden"}, {"type": "file", "file": map[string]any{"file_data": "hidden"}}, {"type": "input_audio"}} {
		if _, _, e := validateRequest(map[string]any{"input": []any{unsafe}}, "/v1/responses"); e == nil {
			t.Fatal("unbounded non-text input accepted")
		}
	}
}
func TestPostgresConcurrentGrantsAndLastBalance(t *testing.T) {
	dsn := os.Getenv("ORCABOX_TEST_DSN")
	if dsn == "" {
		t.Skip("dedicated PostgreSQL DSN required")
	}
	if !strings.Contains(dsn, "dbname=orcabox_test ") {
		t.Fatal("isolated test database required")
	}
	base, e := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if e != nil {
		t.Fatal(e)
	}
	schema := "gateway_" + randomKey()[:16]
	if e := base.Exec("CREATE SCHEMA " + schema).Error; e != nil {
		t.Fatal(e)
	}
	db, e := gorm.Open(postgres.Open(dsn+" search_path="+schema), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if e != nil {
		t.Fatal(e)
	}
	sql, _ := db.DB()
	sql.SetMaxOpenConns(8)
	sql.SetMaxIdleConns(8)
	t.Cleanup(func() {
		sql.Close()
		base.Exec("DROP SCHEMA " + schema + " CASCADE")
		connection, _ := base.DB()
		connection.Close()
	})
	if e = db.AutoMigrate(&model.User{}, &model.Token{}); e != nil {
		t.Fatal(e)
	}
	l := &Ledger{DB: db}
	if e = l.Migrate(); e != nil {
		t.Fatal(e)
	}
	external := "test-" + randomKey()[:12]
	order := "order-" + randomKey()[:12]
	var wg sync.WaitGroup
	errs := make(chan error, 100)
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, e := l.Grant(order, external, 100, 30); errs <- e }()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	a, e := l.Account(external)
	if e != nil || a.Available != 100*Unit {
		t.Fatal("concurrent duplicate grant")
	}
	successes := make(chan Request, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, e := l.Reserve(external, "m", "v1", 100*Unit)
			if e == nil {
				successes <- r
			} else if !errors.Is(e, ErrFunds) {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	close(successes)
	if len(successes) != 1 {
		t.Fatal("last balance sold more than once")
	}
	for r := range successes {
		if e = l.Settle(r.ID, &Usage{Input: 100}, Rate{Input: "10", Output: "50", CacheRead: "0.5", CacheWrite: "12.5"}, false); e != nil {
			t.Fatal(e)
		}
	}
	a, _ = l.Account(external)
	if a.Available != 100*Unit-1000 || a.Frozen != 0 || a.Used != 1000 {
		t.Fatal("concurrent ledger invariant failed")
	}
}

func TestRefundFreezeCannotBeClearedByKeyStatus(t *testing.T) {
	l := testLedger(t)
	l.Grant("order-refund", "buyer-refund", 100, 30)
	r, e := l.Reserve("buyer-refund", "m", "v1", Unit)
	if e != nil {
		t.Fatal(e)
	}
	if e = l.Revoke("order-refund", "buyer-refund"); !errors.Is(e, ErrPendingRefund) {
		t.Fatal("refund bypassed in-flight settlement")
	}
	l.Disable("buyer-refund", false)
	if _, e = l.Reserve("buyer-refund", "m", "v1", Unit); !errors.Is(e, ErrFunds) {
		t.Fatal("buyer cleared administrator refund freeze")
	}
	l.Settle(r.ID, &Usage{Input: 10}, Rate{Input: "10", Output: "50", CacheRead: "0.5", CacheWrite: "12.5"}, false)
	if e = l.Revoke("order-refund", "buyer-refund"); e != nil {
		t.Fatal(e)
	}
	a, _ := l.Account("buyer-refund")
	if !a.AdminFrozen || a.Available != 0 {
		t.Fatal("refund clawback incomplete")
	}
}

func TestRefundBeforeDeliveryTombstoneAndReplay(t *testing.T) {
	l := testLedger(t)
	for i := 0; i < 100; i++ {
		if e := l.Revoke("not-delivered", "buyer"); e != nil {
			t.Fatal(e)
		}
	}
	if _, e := l.Grant("not-delivered", "buyer", 100, 30); !errors.Is(e, ErrRevoked) {
		t.Fatal("delayed delivery resurrected refunded order", e)
	}
	if _, e := l.Grant("not-delivered", "another", 100, 30); !errors.Is(e, ErrConflict) {
		t.Fatal("refund tombstone changed ownership")
	}
	if _, e := l.Account("buyer"); !errors.Is(e, gorm.ErrRecordNotFound) {
		t.Fatal("refund created spendable account")
	}
}

func TestRefundDoesNotRemoveNewEpochCredits(t *testing.T) {
	l := testLedger(t)
	now := time.Now().UTC()
	l.Now = func() time.Time { return now }
	l.Grant("old-order", "buyer", 100, 30)
	now = now.Add(31 * 24 * time.Hour)
	l.Grant("new-order", "buyer", 100, 30)
	if e := l.Revoke("old-order", "buyer"); e != nil {
		t.Fatal(e)
	}
	a, _ := l.Account("buyer")
	if a.Available != 100*Unit || !a.AdminFrozen {
		t.Fatal("refund of expired epoch removed new purchase")
	}
	if _, e := l.Grant("third-order", "buyer", 100, 30); !errors.Is(e, ErrPendingRefund) {
		t.Fatal("refund-frozen account accepted new purchase")
	}
}
