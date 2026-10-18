package orcabox

import (
	"gorm.io/gorm"
	"time"
)

// Early development ledgers predate Grant.Epoch. Reconstruct only that missing
// metadata from immutable grant timestamps, verifying the account epoch. Never
// guess which new-cycle credits an old refund may withdraw.
func (l *Ledger) backfillGrantEpochs() error {
	var buyers []string
	if e := l.DB.Model(&Grant{}).Where("epoch = 0").Distinct("external_id").Pluck("external_id", &buyers).Error; e != nil {
		return e
	}
	for _, buyer := range buyers {
		if e := l.DB.Transaction(func(tx *gorm.DB) error {
			if e := lockBuyer(tx, buyer); e != nil {
				return e
			}
			a, e := l.lock(tx, buyer)
			if e != nil {
				return e
			}
			var rows []Grant
			if e := tx.Where("external_id = ?", buyer).Order("created_at, order_no").Find(&rows).Error; e != nil {
				return e
			}
			epoch := int64(0)
			var expires time.Time
			changed := 0
			for _, g := range rows {
				if g.Credits != 100 || g.Days != 30 {
					return ErrConflict
				}
				if !expires.After(g.CreatedAt) {
					epoch++
					expires = g.CreatedAt
				}
				expires = expires.Add(time.Duration(g.Days) * 24 * time.Hour)
				if g.Epoch == 0 {
					if e := tx.Model(&Grant{}).Where("order_no = ? AND epoch = 0", g.OrderNo).Update("epoch", epoch).Error; e != nil {
						return e
					}
					changed++
				} else if g.Epoch != epoch {
					return ErrConflict
				}
			}
			if epoch != a.Epoch {
				return ErrConflict
			}
			if changed > 0 {
				return tx.Create(&Audit{ExternalID: buyer, Action: "migration.grant_epoch_backfill", CreatedAt: l.now()}).Error
			}
			return nil
		}); e != nil {
			return e
		}
	}
	return nil
}
