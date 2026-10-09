package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/rand"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	gormlogger "gorm.io/gorm/logger"
)

var ErrUpstreamAccountUnavailable = errors.New("upstream account unavailable")
var ErrUpstreamCredentialConflict = errors.New("upstream credential version conflict")

// Credentials remain in Channel.Key. The private digest identifies unchanged
// credentials during reordering; it is never the account's public identity.
type UpstreamAccount struct {
	ID                string `json:"id" gorm:"size:36;primaryKey"`
	ChannelID         int    `json:"channel_id" gorm:"not null;index"`
	CredentialDigest  string `json:"-" gorm:"size:64;not null"`
	CredentialVersion int64  `json:"credential_version" gorm:"not null"`
	RetiredAt         int64  `json:"retired_at" gorm:"not null"`
}

// A binding is an optimization for routing, never authentication or permission.
// Its digest includes the authenticated user and the permitted routing scope.
type UpstreamSessionBinding struct {
	Digest    string `json:"-" gorm:"size:64;primaryKey"`
	UserID    int    `json:"user_id" gorm:"not null;index"`
	RuleName  string `json:"rule_name" gorm:"type:text;not null"`
	ChannelID int    `json:"channel_id" gorm:"not null"`
	AccountID string `json:"account_id" gorm:"size:36;not null"`
	ExpiresAt int64  `json:"expires_at" gorm:"not null;index"`
}

type UpstreamAccountSelection struct {
	Channel Channel
	Account UpstreamAccount
	Key     string
	Index   int
}

func UpstreamScopeDigest(scope string) string {
	digest := sha256.Sum256([]byte(scope))
	return hex.EncodeToString(digest[:])
}

// Channel row locking serializes registry changes and polling with rotation.
// SQLite acquires its write lock before reading a snapshot.
func upstreamChannelTx(tx *gorm.DB, id int) (Channel, error) {
	var channel Channel
	if tx.Dialector.Name() == "sqlite" {
		if err := tx.Model(&Channel{}).Where("id = ?", id).UpdateColumn("id", gorm.Expr("id")).Error; err != nil {
			return channel, err
		}
	}
	err := lockForUpdate(tx).First(&channel, id).Error
	return channel, err
}

func upstreamAccountsTx(tx *gorm.DB, channel Channel, now int64) ([]UpstreamAccount, []string, error) {
	keys := channel.GetKeys()
	if !channel.ChannelInfo.IsMultiKey {
		keys = []string{channel.Key}
	}
	var existing []UpstreamAccount
	if err := tx.Where("channel_id = ? AND retired_at = 0", channel.Id).Find(&existing).Error; err != nil {
		return nil, nil, err
	}
	accounts := make([]UpstreamAccount, len(keys))
	used := make(map[string]bool, len(existing))
	seen := make(map[string]bool, len(keys))
	for i, key := range keys {
		if key == "" {
			return nil, nil, ErrUpstreamAccountUnavailable
		}
		digest := UpstreamScopeDigest(key)
		if seen[digest] {
			return nil, nil, ErrUpstreamCredentialConflict
		}
		seen[digest] = true
		for _, account := range existing {
			if account.CredentialDigest == digest {
				accounts[i] = account
				used[account.ID] = true
				break
			}
		}
		if accounts[i].ID == "" {
			accounts[i] = UpstreamAccount{ID: uuid.NewString(), ChannelID: channel.Id, CredentialDigest: digest, CredentialVersion: 1}
			if err := tx.Create(&accounts[i]).Error; err != nil {
				return nil, nil, err
			}
		}
	}
	for _, account := range existing {
		if !used[account.ID] {
			if err := tx.Model(&account).Update("retired_at", now).Error; err != nil {
				return nil, nil, err
			}
		}
	}
	return accounts, keys, nil
}

func GetUpstreamSessionBinding(db *gorm.DB, userID int, scope string, now int64) (UpstreamSessionBinding, error) {
	var binding UpstreamSessionBinding
	err := db.Where("digest = ? AND user_id = ? AND expires_at > ?", UpstreamScopeDigest(scope), userID, now).First(&binding).Error
	return binding, err
}

// A strict binding is claimed before sending any request, so concurrent first
// requests cannot both establish different accounts. Failed attempts retain it.
type UpstreamAccountRequest struct {
	ChannelID       int
	UserID          int
	Scope           string
	RuleName        string
	TTLSeconds      int64
	Strict          bool
	UseBinding      bool
	PinnedAccountID string
	Now             int64
}

func SelectUpstreamAccount(db *gorm.DB, request UpstreamAccountRequest) (UpstreamAccountSelection, error) {
	var selected UpstreamAccountSelection
	channelID, userID := request.ChannelID, request.UserID
	scope, rule, ttl, strict, useBinding, now := request.Scope, request.RuleName, request.TTLSeconds, request.Strict, request.UseBinding, request.Now
	if db == nil || channelID <= 0 || userID <= 0 || now <= 0 || ttl <= 0 || ttl > 31536000 {
		return selected, ErrUpstreamAccountUnavailable
	}

	err := db.Transaction(func(tx *gorm.DB) error {
		var binding UpstreamSessionBinding
		if scope != "" {
			binding = UpstreamSessionBinding{Digest: UpstreamScopeDigest(scope), UserID: userID, RuleName: rule}
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&binding).Error; err != nil {
				return err
			}
			if err := lockForUpdate(tx).First(&binding, "digest = ?", binding.Digest).Error; err != nil {
				return err
			}
			if binding.UserID != userID {
				return ErrUpstreamAccountUnavailable
			}
			if strict && binding.ExpiresAt > now {
				channelID = binding.ChannelID
			}
		}
		channel, err := upstreamChannelTx(tx, channelID)
		if err != nil {
			return err
		}
		if channel.Status != common.ChannelStatusEnabled {
			return ErrUpstreamAccountUnavailable
		}
		accounts, keys, err := upstreamAccountsTx(tx, channel, now)
		if err != nil {
			return err
		}
		available := make([]int, 0, len(keys))
		bound := -1
		for i, account := range accounts {
			status, exists := channel.ChannelInfo.MultiKeyStatusList[i]
			if channel.ChannelInfo.IsMultiKey && exists && status != common.ChannelStatusEnabled {
				continue
			}
			available = append(available, i)
			if binding.ExpiresAt > now && binding.ChannelID == channel.Id && binding.AccountID == account.ID {
				bound = i
			}
		}
		if len(available) == 0 || strict && binding.ExpiresAt > now && bound < 0 {
			return ErrUpstreamAccountUnavailable
		}
		index := bound
		if request.PinnedAccountID != "" {
			pinned := -1
			for _, candidate := range available {
				if accounts[candidate].ID == request.PinnedAccountID {
					pinned = candidate
					break
				}
			}
			if pinned < 0 || strict && binding.ExpiresAt > now && binding.AccountID != request.PinnedAccountID {
				return ErrUpstreamAccountUnavailable
			}
			index = pinned
		}
		if request.PinnedAccountID == "" && (index < 0 || !useBinding && !strict) {
			index = available[0]
			switch channel.ChannelInfo.MultiKeyMode {
			case constant.MultiKeyModeRandom:
				index = available[rand.Intn(len(available))]
			case constant.MultiKeyModePolling:
				start := channel.ChannelInfo.MultiKeyPollingIndex
				if start < 0 || start >= len(keys) {
					start = 0
				}
				for offset := range keys {
					candidate := (start + offset) % len(keys)
					status, exists := channel.ChannelInfo.MultiKeyStatusList[candidate]
					if !exists || status == common.ChannelStatusEnabled {
						index = candidate
						break
					}
				}
				channel.ChannelInfo.MultiKeyPollingIndex = (index + 1) % len(keys)
				if err := tx.Model(&channel).Update("channel_info", channel.ChannelInfo).Error; err != nil {
					return err
				}
			}
		}

		selected = UpstreamAccountSelection{Channel: channel, Account: accounts[index], Key: keys[index], Index: index}
		if strict && scope != "" && binding.ExpiresAt <= now {
			return tx.Model(&binding).Updates(map[string]any{"channel_id": channel.Id, "account_id": accounts[index].ID, "expires_at": now + ttl}).Error
		}
		return nil
	})
	return selected, err
}

func RotateUpstreamCredential(db *gorm.DB, channelID int, accountID string, version int64, key string, now int64) (UpstreamAccount, error) {
	var account UpstreamAccount
	if version <= 0 || version >= common.MaxWalletQuota || len(key) == 0 || len(key) > 65535 || strings.ContainsAny(key, "\r\n") {
		return account, ErrUpstreamCredentialConflict
	}
	err := db.Transaction(func(tx *gorm.DB) error {
		channel, err := upstreamChannelTx(tx, channelID)
		if err != nil {
			return err
		}
		accounts, keys, err := upstreamAccountsTx(tx, channel, now)
		if err != nil {
			return err
		}
		index := -1
		for i, candidate := range accounts {
			if candidate.ID == accountID {
				account = candidate
				index = i
			}
			if candidate.ID != accountID && candidate.CredentialDigest == UpstreamScopeDigest(key) {
				return ErrUpstreamCredentialConflict
			}
		}
		if index >= 0 && account.CredentialVersion == version+1 && account.CredentialDigest == UpstreamScopeDigest(key) {
			return nil
		}
		if index < 0 || account.CredentialVersion != version {
			return ErrUpstreamCredentialConflict
		}
		if account.CredentialDigest == UpstreamScopeDigest(key) {
			return nil
		}
		keys[index] = key
		if channel.ChannelInfo.IsMultiKey {
			if strings.HasPrefix(strings.TrimSpace(channel.Key), "[") {
				raw := make([]json.RawMessage, len(keys))
				for i, value := range keys {
					raw[i] = json.RawMessage(value)
				}
				encoded, err := common.Marshal(raw)
				if err != nil {
					return err
				}
				channel.Key = string(encoded)
			} else {
				channel.Key = strings.Join(keys, "\n")
			}
		} else {
			channel.Key = key
		}
		// This update includes usable credentials; exclude only this statement
		// from SQL tracing. The caller retains the non-secret management audit.
		if err := tx.Session(&gorm.Session{Logger: gormlogger.Discard}).Model(&channel).Update("key", channel.Key).Error; err != nil {
			return err
		}
		account.CredentialVersion++
		account.CredentialDigest = UpstreamScopeDigest(key)
		return tx.Model(&account).Select("credential_version", "credential_digest").Updates(&account).Error
	})
	return account, err
}

func ListUpstreamAccounts(db *gorm.DB, channelID int, now int64) ([]UpstreamAccount, error) {
	var accounts []UpstreamAccount
	err := db.Transaction(func(tx *gorm.DB) error {
		channel, err := upstreamChannelTx(tx, channelID)
		if err != nil {
			return err
		}
		accounts, _, err = upstreamAccountsTx(tx, channel, now)
		return err
	})
	return accounts, err
}

// Automatic OAuth refresh already names the original credential. Resolve its
// stable identity before rotating; a concurrent edit cannot be overwritten.
func ReplaceRefreshedUpstreamCredential(db *gorm.DB, channelID int, original, replacement string, now int64) error {
	accounts, err := ListUpstreamAccounts(db, channelID, now)
	if err != nil {
		return err
	}
	for _, account := range accounts {
		if account.CredentialDigest == UpstreamScopeDigest(original) {
			_, err := RotateUpstreamCredential(db, channelID, account.ID, account.CredentialVersion, replacement, now)
			return err
		}
	}
	return ErrUpstreamCredentialConflict
}

func ResolveTaskUpstreamAccount(db *gorm.DB, task *Task, now int64) (UpstreamAccountSelection, error) {
	if task == nil || task.PrivateData.AccountID == "" {
		return UpstreamAccountSelection{}, ErrUpstreamAccountUnavailable
	}
	return SelectUpstreamAccount(db, UpstreamAccountRequest{ChannelID: task.ChannelId, UserID: task.UserId, TTLSeconds: 3600, PinnedAccountID: task.PrivateData.AccountID, Now: now})
}
