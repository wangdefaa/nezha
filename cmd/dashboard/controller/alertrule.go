package controller

import (
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/copier"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

// List Alert rules
// @Summary List Alert rules
// @Security BearerAuth
// @Schemes
// @Description List Alert rules
// @Tags auth required
// @Param id query uint false "Resource ID"
// @Produce json
// @Success 200 {object} model.CommonResponse[[]model.AlertRule]
// @Router /alert-rule [get]
func listAlertRule(c *gin.Context) ([]*model.AlertRule, error) {
	singleton.AlertsLock.RLock()
	defer singleton.AlertsLock.RUnlock()

	var ar []*model.AlertRule
	if err := copier.Copy(&ar, &singleton.Alerts); err != nil {
		return nil, err
	}
	return ar, nil
}

// Add Alert Rule
// @Summary Add Alert Rule
// @Security BearerAuth
// @Schemes
// @Description Add Alert Rule
// @Tags auth required
// @Accept json
// @param request body model.AlertRuleForm true "AlertRuleForm"
// @Produce json
// @Success 200 {object} model.CommonResponse[uint64]
// @Router /alert-rule [post]
func createAlertRule(c *gin.Context) (uint64, error) {
	var arf model.AlertRuleForm
	var r model.AlertRule

	if err := c.ShouldBindJSON(&arf); err != nil {
		return 0, err
	}

	r.UserID = getUid(c)
	applyAlertRuleForm(&r, &arf)
	if err := validateRule(c, &r); err != nil {
		return 0, err
	}

	if err := singleton.DB.Create(&r).Error; err != nil {
		return 0, newGormError("%v", err)
	}

	singleton.OnRefreshOrAddAlert(&r)
	return r.ID, nil
}

// Update Alert Rule
// @Summary Update Alert Rule
// @Security BearerAuth
// @Schemes
// @Description Update Alert Rule
// @Tags auth required
// @Accept json
// @param id path uint true "Alert ID"
// @param request body model.AlertRuleForm true "AlertRuleForm"
// @Produce json
// @Success 200 {object} model.CommonResponse[any]
// @Router /alert-rule/{id} [patch]
func updateAlertRule(c *gin.Context) (any, error) {
	id, err := paramID(c)
	if err != nil {
		return nil, err
	}

	var arf model.AlertRuleForm
	if err := c.ShouldBindJSON(&arf); err != nil {
		return nil, err
	}

	var r model.AlertRule
	if err := singleton.DB.First(&r, id).Error; err != nil {
		return nil, singleton.Localizer.ErrorT("alert id %d does not exist", id)
	}

	if !r.HasPermission(c) {
		return nil, singleton.Localizer.ErrorT("permission denied")
	}

	applyAlertRuleForm(&r, &arf)
	if err := validateRule(c, &r); err != nil {
		return nil, err
	}

	if err := singleton.DB.Save(&r).Error; err != nil {
		return nil, newGormError("%v", err)
	}

	singleton.OnRefreshOrAddAlert(&r)
	return r.ID, nil
}

// applyAlertRuleForm 把表单字段写入 r（不动 ID/UserID）。
func applyAlertRuleForm(r *model.AlertRule, arf *model.AlertRuleForm) {
	r.Name = arf.Name
	r.Rules = arf.Rules
	r.NotificationGroupID = arf.NotificationGroupID
	r.TriggerMode = arf.TriggerMode
	enable := arf.Enable
	r.Enable = &enable
}

// Batch delete Alert rules
// @Summary Batch delete Alert rules
// @Security BearerAuth
// @Schemes
// @Description Batch delete Alert rules
// @Tags auth required
// @Accept json
// @param request body []uint64 true "id list"
// @Produce json
// @Success 200 {object} model.CommonResponse[any]
// @Router /batch-delete/alert-rule [post]
func batchDeleteAlertRule(c *gin.Context) (any, error) {
	var ar []uint64
	if err := c.ShouldBindJSON(&ar); err != nil {
		return nil, err
	}

	var ars []model.AlertRule
	if err := singleton.DB.Where("id in (?)", ar).Find(&ars).Error; err != nil {
		return nil, err
	}

	for _, a := range ars {
		if !a.HasPermission(c) {
			return nil, singleton.Localizer.ErrorT("permission denied")
		}
	}

	if err := singleton.DB.Unscoped().Delete(&model.AlertRule{}, "id in (?)", ar).Error; err != nil {
		return nil, newGormError("%v", err)
	}

	singleton.OnDeleteAlert(ar)
	return nil, nil
}

func validateRule(c *gin.Context, r *model.AlertRule) error {
	if !r.HasPermission(c) {
		return singleton.Localizer.ErrorT("permission denied")
	}
	if len(r.Rules) == 0 {
		return singleton.Localizer.ErrorT("need to configure at least a single rule")
	}
	for _, rule := range r.Rules {
		if err := validateRuleItem(rule); err != nil {
			return err
		}
	}
	return assertOwnsNotificationGroup(c, r.NotificationGroupID)
}

// validateRuleItem 校验单条规则：类型白名单、覆盖范围与 duration 上下限（上限防持久化后转 int 回绕，毒化告警协程）。
func validateRuleItem(rule *model.Rule) error {
	if rule == nil {
		return singleton.Localizer.ErrorT("rule is not set")
	}
	if !rule.IsSupportedType() {
		return singleton.Localizer.ErrorT("unsupported rule type")
	}
	switch rule.Cover {
	case model.RuleCoverAll, model.RuleCoverIgnoreAll:
	default:
		return singleton.Localizer.ErrorT("permission denied")
	}
	if rule.IsTransferDurationRule() {
		return validateCycleRule(rule)
	}
	if rule.Duration < 3 {
		return singleton.Localizer.ErrorT("duration need to be at least 3")
	}
	if rule.Duration > model.MaxAlertRuleDuration {
		return singleton.Localizer.ErrorT("duration is too large")
	}
	return nil
}

// validateCycleRule 校验周期流量规则的周期参数（上限防日历换算溢出致除零或死循环）。
func validateCycleRule(rule *model.Rule) error {
	if rule.CycleInterval < 1 {
		return singleton.Localizer.ErrorT("cycle_interval need to be at least 1")
	}
	if rule.CycleInterval > model.MaxAlertRuleCycleInterval {
		return singleton.Localizer.ErrorT("cycle_interval is too large")
	}
	if rule.CycleStart == nil {
		return singleton.Localizer.ErrorT("cycle_start is not set")
	}
	if rule.CycleStart.After(time.Now()) {
		return singleton.Localizer.ErrorT("cycle_start is a future value")
	}
	return nil
}
