package orcabox

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"gorm.io/gorm/clause"
)

// Stateless Responses reasoning is opaque. Its encrypted bytes alone cannot
// prove the number of tokens inside. Only replay items emitted for this buyer
// with verified terminal usage; keep hashes and token ceilings, not contents.
type OpaqueBudget struct {
	AccountID     int    `gorm:"primaryKey"`
	Digest        string `gorm:"primaryKey;size:64"`
	OutputCeiling int64
	CreatedAt     time.Time `gorm:"index"`
}

func (OpaqueBudget) TableName() string { return "orcabox_opaque_budgets" }

type SettlementMetadata struct {
	RequestID     string
	ResponseID    string
	OpaqueDigests []string
}

func opaqueDigest(item any) string {
	b, e := json.Marshal(item)
	if e != nil {
		return ""
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func (l *Ledger) opaqueCeiling(accountID int, item map[string]any) (int64, error) {
	var row OpaqueBudget
	if e := l.DB.Where("account_id = ? AND digest = ? AND created_at > ?", accountID, opaqueDigest(item), l.now().Add(-30*24*time.Hour)).First(&row).Error; e != nil {
		return 0, errors.New("思考记录尚未核验或已过期，请新建会话")
	}
	return row.OutputCeiling, nil
}

func recordOpaque(txLedger *Ledger, r Request, u *Usage, metadata SettlementMetadata) error {
	if u == nil || u.Output < 1 || u.Output > 8192 {
		return nil
	}
	for _, digest := range metadata.OpaqueDigests {
		if len(digest) != 64 {
			return errors.New("invalid opaque digest")
		}
		row := OpaqueBudget{AccountID: r.AccountID, Digest: digest, OutputCeiling: u.Output, CreatedAt: txLedger.now()}
		if e := txLedger.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; e != nil {
			return e
		}
	}
	return nil
}
