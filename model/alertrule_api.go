package model

type AlertRuleForm struct {
	Name                string  `json:"name" minLength:"1"`
	Rules               []*Rule `json:"rules"`
	NotificationGroupID uint64  `json:"notification_group_id"`
	TriggerMode         uint8   `json:"trigger_mode" default:"0"`
	Enable              bool    `json:"enable" validate:"optional"`
}
