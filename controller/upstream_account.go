package controller

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service/authz"
	"github.com/gin-gonic/gin"
)

func authorizeChannelAccountAdmin(c *gin.Context, permission authz.Permission) bool {
	user, err := model.GetUserById(c.GetInt("id"), false)
	if err != nil || user.Status != common.UserStatusEnabled || user.Role < common.RoleAdminUser || !authz.Can(user.Id, user.Role, permission) {
		c.JSON(http.StatusForbidden, gin.H{"success": false, "code": "upstream_account_permission_denied"})
		return false
	}
	return true
}

func ListChannelAccounts(c *gin.Context) {
	if !authorizeChannelAccountAdmin(c, authz.ChannelRead) {
		return
	}
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"success": false})
		return
	}
	accounts, err := model.ListUpstreamAccounts(model.DB, id, common.GetTimestamp())
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "code": "upstream_account_unavailable"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": accounts})
}

func RotateChannelAccountCredential(c *gin.Context) {
	if !authorizeChannelAccountAdmin(c, authz.ChannelSensitiveWrite) {
		return
	}
	id, err := strconv.Atoi(c.Param("id"))
	var input struct {
		Version int64  `json:"credential_version"`
		Key     string `json:"key"`
	}
	if err != nil || id <= 0 || c.ShouldBindJSON(&input) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false})
		return
	}
	account, err := model.RotateUpstreamCredential(model.DB, id, c.Param("account_id"), input.Version, input.Key, common.GetTimestamp())
	if err != nil {
		status := http.StatusServiceUnavailable
		if errors.Is(err, model.ErrUpstreamCredentialConflict) {
			status = http.StatusConflict
		}
		c.JSON(status, gin.H{"success": false, "code": "upstream_credential_conflict"})
		return
	}
	recordManageAudit(c, "channel.credential_rotate", map[string]any{"id": id, "account_id": account.ID, "credential_version": account.CredentialVersion})
	model.InitChannelCache()
	c.JSON(http.StatusOK, gin.H{"success": true, "data": account})
}
