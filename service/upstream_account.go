package service

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
)

// ResolveUpstreamAccount runs before the chosen channel config is installed.
// The database may return a competing first request's strict binding winner;
// its authorization and request filters are checked again before using it.
func ResolveUpstreamAccount(c *gin.Context, channel *model.Channel, modelName string) (*model.Channel, string, int, *types.NewAPIError) {
	if channel.Id <= 0 || c.GetInt("id") <= 0 {
		key, index, err := channel.GetNextEnabledKey()
		return channel, key, index, err
	}
	if c.GetBool("upstream_binding_read_error") {
		return channel, "", 0, types.NewErrorWithStatusCode(errors.New("binding unavailable"), "upstream_account_storage_unavailable", http.StatusServiceUnavailable, types.ErrOptionWithSkipRetry())
	}
	meta, matched := getChannelAffinityMeta(c)
	mode := RequestPolicy(c).SessionMode
	scope, rule := "", ""
	ttl := int64(3600)
	if matched && mode != "off" {
		scope, rule = meta.CacheKey, meta.RuleName
		if meta.TTLSeconds > 0 {
			ttl = int64(meta.TTLSeconds)
		}
	}

	pinnedAccountID := ""
	if tasks, ok := common.GetContextKeyType[[]*model.Task](c, constant.ContextKeyOriginTasks); ok {
		for _, task := range tasks {
			if task == nil || task.PrivateData.AccountID == "" {
				continue
			}
			if task.UserId != c.GetInt("id") || task.ChannelId != channel.Id || pinnedAccountID != "" && pinnedAccountID != task.PrivateData.AccountID {
				return channel, "", 0, types.NewErrorWithStatusCode(model.ErrUpstreamAccountUnavailable, "origin_task_account_conflict", http.StatusBadRequest, types.ErrOptionWithSkipRetry())
			}
			pinnedAccountID = task.PrivateData.AccountID
		}
	}
	selected, err := model.SelectUpstreamAccount(model.DB, model.UpstreamAccountRequest{ChannelID: channel.Id, UserID: c.GetInt("id"), Scope: scope, RuleName: rule, TTLSeconds: ttl, Strict: mode == "strict", UseBinding: RequestPolicy(c).Attempts <= 1, PinnedAccountID: pinnedAccountID, Now: common.GetTimestamp()})
	if err != nil {
		code := "upstream_account_unavailable"
		if !errors.Is(err, model.ErrUpstreamAccountUnavailable) && !errors.Is(err, model.ErrUpstreamCredentialConflict) {
			code = "upstream_account_storage_unavailable"
		}
		return channel, "", 0, types.NewErrorWithStatusCode(errors.New(code), types.ErrorCode(code), http.StatusServiceUnavailable, types.ErrOptionWithSkipRetry())
	}
	group := common.GetContextKeyString(c, constant.ContextKeyUsingGroup)
	if group == "" {
		group = meta.UsingGroup
	}
	if group == "auto" {
		group = common.GetContextKeyString(c, constant.ContextKeyAutoGroup)
	}
	if ok, _ := model.ChannelSatisfiesFilters(&selected.Channel, modelName, GetChannelConstraints(c).Filters); !ok || group != "" && modelName != "" && !model.IsChannelEnabledForGroupModelFresh(group, modelName, selected.Channel.Id) {
		return channel, "", 0, types.NewErrorWithStatusCode(model.ErrUpstreamAccountUnavailable, "upstream_account_unavailable", http.StatusServiceUnavailable, types.ErrOptionWithSkipRetry())
	}
	if pin, exists, _ := GetChannelConstraints(c).ResolvedPin(); exists && selected.Channel.Id != pin.ChannelId {
		return channel, "", 0, types.NewErrorWithStatusCode(model.ErrUpstreamAccountUnavailable, "strict_session_pin_conflict", http.StatusServiceUnavailable, types.ErrOptionWithSkipRetry())
	}
	c.Set("upstream_account_id", selected.Account.ID)
	c.Set("upstream_credential_version", selected.Account.CredentialVersion)
	RequestPolicy(c).AddEvent(PolicyEvent{ChannelID: selected.Channel.Id, AccountID: selected.Account.ID, CredentialVersion: selected.Account.CredentialVersion, Decision: PolicyDecision{Action: "select", Reason: "upstream_account_selected", Source: "account_registry"}})
	if selected.Channel.Id != channel.Id {
		RequestPolicy(c).AddEvent(PolicyEvent{ChannelID: selected.Channel.Id, Decision: PolicyDecision{Action: "bind", Reason: "concurrent_session_binding", Source: "session_rule"}})
	}
	return &selected.Channel, selected.Key, selected.Index, nil
}

func scopedAffinityKey(c *gin.Context, rule operation_setting.ChannelAffinityRule, modelName, group, value string) string {
	modelScope := ""
	if rule.IncludeModelName {
		modelScope = modelName
	}
	encoded, _ := common.Marshal([]any{c.GetInt("id"), group, rule.Name, modelScope, value})
	return fmt.Sprintf("%s:u%d:%s", rule.Name, c.GetInt("id"), model.UpstreamScopeDigest(string(encoded)))
}
