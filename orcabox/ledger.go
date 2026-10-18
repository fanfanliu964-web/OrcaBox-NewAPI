// Package orcabox owns paid entitlements inside the New API database.
// Native wallet quota is not a second customer balance: managed requests bypass
// its billing path, while keeping New API's authentication and protocol adapters.
package orcabox

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const Unit int64 = 1000000

var ErrFunds = errors.New("insufficient or expired credits")
var ErrPendingRefund = errors.New("refund waits for in-flight settlements")
var ErrConcurrency = errors.New("active request limit reached")
var ErrConflict = errors.New("idempotency facts conflict")
var ErrRevoked = errors.New("order entitlement revoked")
var ErrKeyChanged = errors.New("key changed or disabled")
var ErrAuthUnavailable = errors.New("authentication storage unavailable")

type Account struct {
	ID            int       `gorm:"primaryKey" json:"-"`
	ExternalID    string    `gorm:"uniqueIndex;not null" json:"-"`
	UserID        int       `gorm:"uniqueIndex;not null" json:"-"`
	TokenID       int       `json:"-"`
	KeyGeneration int64     `gorm:"not null;default:0" json:"-"`
	Epoch         int64     `json:"-"`
	Available     int64     `json:"available_units"`
	Frozen        int64     `json:"frozen_units"`
	Used          int64     `json:"used_units"`
	Expired       int64     `json:"expired_units"`
	ExpiresAt     time.Time `json:"expires_at"`
	AdminFrozen   bool      `json:"administratively_frozen"`
	Disabled      bool      `json:"disabled"`
	UpdatedAt     time.Time `json:"updated_at"`
}
type Grant struct {
	OrderNo    string    `gorm:"primaryKey;size:80" json:"order_no"`
	ExternalID string    `gorm:"index;not null" json:"-"`
	Credits    int64     `json:"credits"`
	Days       int       `json:"days"`
	Revoked    bool      `json:"revoked"`
	Epoch      int64     `json:"-"`
	CreatedAt  time.Time `json:"created_at"`
}

// A refund can precede delivery. This tombstone stops a delayed grant forever.
type Revocation struct {
	OrderNo    string `gorm:"primaryKey;size:80"`
	ExternalID string `gorm:"not null"`
	CreatedAt  time.Time
}

func (Revocation) TableName() string { return "orcabox_revocations" }

type Request struct {
	Epoch              int64      `json:"-"`
	ID                 string     `gorm:"primaryKey;size:64" json:"id"`
	AccountID          int        `gorm:"index;not null" json:"-"`
	Model              string     `json:"model"`
	PriceVersion       string     `json:"price_version"`
	RateSnapshot       string     `gorm:"type:text" json:"-"`
	SupplierRequestID  string     `gorm:"size:160" json:"supplier_request_id,omitempty"`
	SupplierResponseID string     `gorm:"size:160" json:"supplier_response_id,omitempty"`
	Reserved           int64      `json:"reserved_units"`
	Charged            int64      `json:"charged_units"`
	Status             string     `gorm:"index" json:"status"`
	Input              int64      `json:"input_tokens"`
	Output             int64      `json:"output_tokens"`
	Cached             int64      `json:"cached_tokens"`
	CacheWrite         int64      `json:"cache_write_tokens"`
	CreatedAt          time.Time  `gorm:"index" json:"created_at"`
	FinishedAt         *time.Time `json:"finished_at"`
}
type Audit struct {
	ID         uint   `gorm:"primaryKey"`
	ExternalID string `gorm:"index"`
	Action     string
	CreatedAt  time.Time
}
type Ledger struct {
	DB  *gorm.DB
	Now func() time.Time
}

func (l *Ledger) now() time.Time {
	if l.Now != nil {
		return l.Now().UTC().Truncate(time.Microsecond)
	}
	return time.Now().UTC().Truncate(time.Microsecond)
}
func randomKey() string {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		panic("secure random unavailable")
	}
	return hex.EncodeToString(b)
}
func (l *Ledger) Migrate() error {
	if e := l.DB.AutoMigrate(&Account{}, &Grant{}, &Request{}, &Audit{}, &PublishedPrices{}, &OpaqueBudget{}, &Revocation{}, &ModelGuard{}, &TestBudget{}, &TestBudgetCorrection{}); e != nil {
		return e
	}
	if e := l.backfillGrantEpochs(); e != nil {
		return e
	}
	return l.repairTestBudget()
}
func lockBuyer(tx *gorm.DB, external string) error {
	if tx.Dialector.Name() == "postgres" {
		return tx.Exec("SELECT pg_advisory_xact_lock(hashtextextended(?, 0))", "orcabox:"+external).Error
	}
	return nil
}
func (l *Ledger) lock(tx *gorm.DB, external string) (Account, error) {
	var a Account
	e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("external_id = ?", external).First(&a).Error
	return a, e
}
func (l *Ledger) expire(a *Account, now time.Time) {
	if !a.ExpiresAt.After(now) {
		a.Expired += a.Available
		a.Available = 0
	}
}
func (l *Ledger) Grant(order, external string, credits int64, days int) (Account, error) {
	var a Account
	if len(order) < 4 || len(order) > 80 || len(external) < 1 || len(external) > 60 || credits != 100 || days != 30 {
		return a, ErrConflict
	}
	e := l.DB.Transaction(func(tx *gorm.DB) error {
		// An advisory lock covers first purchase, before an account row exists.
		if e := lockBuyer(tx, external); e != nil {
			return e
		}
		var revoked Revocation
		if e := tx.First(&revoked, "order_no = ?", order).Error; e == nil {
			if revoked.ExternalID != external {
				return ErrConflict
			}
			return ErrRevoked
		} else if !errors.Is(e, gorm.ErrRecordNotFound) {
			return e
		}
		var old Grant
		e := tx.First(&old, "order_no = ?", order).Error
		if e == nil {
			if old.ExternalID != external || old.Credits != credits || old.Days != days {
				return ErrConflict
			}
			a, e = l.lock(tx, external)
			return e
		}
		if !errors.Is(e, gorm.ErrRecordNotFound) {
			return e
		}
		a, e = l.lock(tx, external)
		if errors.Is(e, gorm.ErrRecordNotFound) {
			pass, e := common.Password2Hash(randomKey())
			if e != nil {
				return e
			}
			u := model.User{Username: "ob_" + randomKey()[:16], Password: pass, DisplayName: "OrcaBox", AffCode: randomKey()[:20], Role: common.RoleCommonUser, Status: common.UserStatusEnabled, Group: "default", Quota: 1}
			if e := tx.Create(&u).Error; e != nil {
				return e
			}
			t := model.Token{UserId: u.Id, Key: randomKey(), Name: "OrcaBox", Status: common.TokenStatusEnabled, UnlimitedQuota: true, ExpiredTime: -1, CreatedTime: l.now().Unix(), Group: "default", ModelLimitsEnabled: true, ModelLimits: "gpt-5.6-sol,gpt-6-sol,gpt-6.1-sol,gpt-6-astra"}
			if e := tx.Create(&t).Error; e != nil {
				return e
			}
			a = Account{ExternalID: external, UserID: u.Id, TokenID: t.Id}
			if e := tx.Create(&a).Error; e != nil {
				return e
			}
		} else if e != nil {
			return e
		}
		if a.AdminFrozen {
			return ErrPendingRefund
		}
		now := l.now()
		if !a.ExpiresAt.After(now) {
			a.Epoch++
		}
		l.expire(&a, now)
		start := a.ExpiresAt
		if start.Before(now) {
			start = now
		}
		a.Available += credits * Unit
		a.ExpiresAt = start.Add(time.Duration(days) * 24 * time.Hour)
		a.UpdatedAt = now
		if e := tx.Save(&a).Error; e != nil {
			return e
		}
		return tx.Create(&Grant{OrderNo: order, ExternalID: external, Credits: credits, Days: days, Epoch: a.Epoch, CreatedAt: now}).Error
	})
	return a, e
}
func (l *Ledger) Reserve(external, modelName, version string, units int64, rates ...Rate) (Request, error) {
	return l.reserve(external, modelName, version, units, nil, rates...)
}

// Admission and rotation share the account lock. A request authenticated before
// rotation cannot reserve funds using that stale credential after rotation.
func (l *Ledger) ReserveAuthenticated(external, modelName, version string, units, generation int64, rates ...Rate) (Request, error) {
	return l.reserve(external, modelName, version, units, &generation, rates...)
}

func (l *Ledger) reserve(external, modelName, version string, units int64, generation *int64, rates ...Rate) (Request, error) {
	r := Request{ID: randomKey(), Model: modelName, PriceVersion: version, Reserved: units, Status: "reserved", CreatedAt: l.now()}
	if len(rates) == 1 {
		snapshot, err := json.Marshal(rates[0])
		if err != nil {
			return r, err
		}
		r.RateSnapshot = string(snapshot)
	}
	if units <= 0 {
		return r, ErrFunds
	}
	e := l.DB.Transaction(func(tx *gorm.DB) error {
		a, e := l.lock(tx, external)
		if e != nil {
			return e
		}
		if generation != nil && a.KeyGeneration != *generation {
			return ErrKeyChanged
		}
		l.expire(&a, l.now())
		if a.Disabled || a.AdminFrozen || !a.ExpiresAt.After(l.now()) || a.Available < units {
			return ErrFunds
		}
		// Redis leases may disappear on restart. The account row lock plus
		// durable active rows also enforce the limit across that failure.
		var active int64
		if e := tx.Model(&Request{}).Where("account_id = ? AND status = ?", a.ID, "reserved").Count(&active).Error; e != nil {
			return e
		}
		if active >= 2 {
			return ErrConcurrency
		}
		if e := commitTestBudget(tx, units); e != nil {
			return e
		}
		a.Available -= units
		a.Frozen += units
		a.UpdatedAt = l.now()
		r.AccountID = a.ID
		r.Epoch = a.Epoch
		if e := tx.Save(&a).Error; e != nil {
			return e
		}
		return tx.Create(&r).Error
	})
	return r, e
}

// Settle is durable and idempotent. A supplier overrun never creates user debt.
func (l *Ledger) Settle(id string, u *Usage, rate Rate, definitiveFailure bool, metadata ...SettlementMetadata) error {
	return l.DB.Transaction(func(tx *gorm.DB) error {
		var r Request
		if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&r, "id = ?", id).Error; e != nil {
			return e
		}
		if r.Status == "settled" || r.Status == "released" || r.Status == "overrun" {
			return nil
		}
		if len(metadata) == 1 {
			r.SupplierRequestID = safeSupplierID(metadata[0].RequestID)
			r.SupplierResponseID = safeSupplierID(metadata[0].ResponseID)
		}
		var a Account
		if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&a, r.AccountID).Error; e != nil {
			return e
		}
		if u == nil && !definitiveFailure && l.now().Sub(r.CreatedAt) < 24*time.Hour {
			r.Status = "pending_usage"
			return tx.Save(&r).Error
		}
		charge := int64(0)
		r.Status = "released"
		if u != nil {
			var e error
			charge, e = rate.Cost(*u)
			if e != nil {
				r.Status = "pending_usage"
				return tx.Save(&r).Error
			}
			r.Input = u.Input
			r.Output = u.Output
			r.Cached = u.Cached
			r.CacheWrite = u.CacheWrite
			r.Status = "settled"
			if charge > r.Reserved {
				if e := correctTestBudget(tx, r, charge); e != nil {
					return e
				}
				charge = r.Reserved
				r.Status = "overrun"
				guard := ModelGuard{Model: r.Model, Locked: true, Reason: "reservation_overrun", CauseVersion: r.PriceVersion}
				if e := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "model"}}, DoUpdates: clause.Assignments(map[string]any{"locked": true, "reason": "reservation_overrun", "cause_version": r.PriceVersion})}).Create(&guard).Error; e != nil {
					return e
				}
			}
			if len(metadata) == 1 {
				if e := recordOpaque(&Ledger{DB: tx, Now: l.Now}, r, u, metadata[0]); e != nil {
					return e
				}
			}
		}
		a.Frozen -= r.Reserved
		a.Used += charge
		if a.ExpiresAt.After(l.now()) && a.Epoch == r.Epoch {
			a.Available += r.Reserved - charge
		} else {
			a.Expired += r.Reserved - charge
		}
		r.Charged = charge
		now := l.now()
		r.FinishedAt = &now
		a.UpdatedAt = now
		if e := tx.Save(&a).Error; e != nil {
			return e
		}
		return tx.Save(&r).Error
	})
}
func (l *Ledger) Account(external string) (Account, error) {
	var a Account
	e := l.DB.Where("external_id = ?", external).First(&a).Error
	if e == nil {
		l.expire(&a, l.now())
	}
	return a, e
}
func (l *Ledger) Key(external string) (string, error) {
	a, e := l.Account(external)
	if e != nil {
		return "", e
	}
	var t model.Token
	e = l.DB.First(&t, a.TokenID).Error
	return "sk-" + t.Key, e
}
func (l *Ledger) Rotate(external string) error {
	return l.DB.Transaction(func(tx *gorm.DB) error {
		a, e := l.lock(tx, external)
		if e != nil {
			return e
		}
		changed := tx.Model(&model.Token{}).Where("id = ? AND user_id = ?", a.TokenID, a.UserID).Update("key", randomKey())
		if changed.Error != nil {
			return changed.Error
		}
		if changed.RowsAffected != 1 {
			return ErrKeyChanged
		}
		return tx.Model(&a).Updates(map[string]any{"key_generation": a.KeyGeneration + 1, "updated_at": l.now()}).Error
	})
}
func (l *Ledger) Disable(external string, disabled bool) error {
	return l.DB.Model(&Account{}).Where("external_id = ?", external).Update("disabled", disabled).Error
}
func (l *Ledger) Revoke(order, external string) error {
	if len(order) < 4 || len(order) > 80 || len(external) < 1 || len(external) > 60 {
		return ErrConflict
	}
	pending := false
	err := l.DB.Transaction(func(tx *gorm.DB) error {
		if e := lockBuyer(tx, external); e != nil {
			return e
		}
		var tombstone Revocation
		if e := tx.First(&tombstone, "order_no = ?", order).Error; e == nil {
			if tombstone.ExternalID != external {
				return ErrConflict
			}
		} else if errors.Is(e, gorm.ErrRecordNotFound) {
			if e := tx.Create(&Revocation{OrderNo: order, ExternalID: external, CreatedAt: l.now()}).Error; e != nil {
				return e
			}
		} else {
			return e
		}
		var g Grant
		if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&g, "order_no = ?", order).Error; e != nil {
			if errors.Is(e, gorm.ErrRecordNotFound) {
				return nil
			}
			return e
		}
		if g.ExternalID != external {
			return ErrConflict
		}
		a, e := l.lock(tx, external)
		if e != nil {
			return e
		}
		if g.Revoked {
			return nil
		}
		a.AdminFrozen = true
		if a.Frozen > 0 {
			pending = true
			return tx.Save(&a).Error
		}
		l.expire(&a, l.now())
		take := g.Credits * Unit
		if g.Epoch != a.Epoch {
			take = 0
		}
		if take > a.Available {
			take = a.Available
		}
		a.Available -= take
		if g.Epoch == a.Epoch {
			a.ExpiresAt = a.ExpiresAt.Add(-time.Duration(g.Days) * 24 * time.Hour)
			if a.ExpiresAt.Before(l.now()) {
				a.ExpiresAt = l.now()
			}
			l.expire(&a, l.now())
		}
		g.Revoked = true
		if e := tx.Save(&a).Error; e != nil {
			return e
		}
		return tx.Save(&g).Error
	})
	if err == nil && pending {
		return ErrPendingRefund
	}
	return err
}

func (l *Ledger) Recover() error {
	if e := l.recoverSpool(); e != nil {
		return e
	}
	var rows []Request
	if e := l.DB.Where("status IN ? AND created_at < ?", []string{"reserved", "pending_usage"}, l.now().Add(-4*time.Minute)).Limit(100).Find(&rows).Error; e != nil {
		return e
	}
	for _, r := range rows {
		if e := l.Settle(r.ID, nil, Rate{}, false); e != nil {
			return fmt.Errorf("settlement recovery failed")
		}
	}
	return nil
}

func (Account) TableName() string { return "orcabox_accounts" }
func (Grant) TableName() string   { return "orcabox_grants" }
func (Request) TableName() string { return "orcabox_requests" }
func (Audit) TableName() string   { return "orcabox_audits" }
