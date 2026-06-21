package model

import (
	"encoding/json"

	"gorm.io/gorm"
)

// DynamicConfig 运行时可经后台修改、并持久化到数据库的配置快照。
// 启动引导所需的 bootstrap 配置（端口、数据库、JWT、密钥等）仍留在 yaml/env，不入库。
type DynamicConfig struct {
	ConfigForGuests
	ConfigDashboard
	Oauth2 map[string]*Oauth2Config `json:"oauth2,omitempty"`
}

// SettingStore 单行（id=1）存储 DynamicConfig 的 JSON。
type SettingStore struct {
	ID   uint8  `gorm:"primaryKey" json:"id"`
	Data string `gorm:"type:text" json:"-"`
}

// DynamicSnapshot 导出当前动态配置。
func (c *Config) DynamicSnapshot() DynamicConfig {
	return DynamicConfig{
		ConfigForGuests: c.ConfigForGuests,
		ConfigDashboard: c.ConfigDashboard,
		Oauth2:          c.Oauth2,
	}
}

// applyDynamic 用快照覆盖运行时动态配置。
func (c *Config) applyDynamic(d DynamicConfig) {
	c.ConfigForGuests = d.ConfigForGuests
	c.ConfigDashboard = d.ConfigDashboard
	c.Oauth2 = d.Oauth2
}

// LoadDynamicFromDB 从数据库加载动态配置；首次（无记录）时把 yaml 现值迁移入库。
func (c *Config) LoadDynamicFromDB(db *gorm.DB) error {
	var s SettingStore
	// 用 Find 而非 First，避免空表时打印 ErrRecordNotFound 噪音日志。
	if err := db.Limit(1).Find(&s).Error; err != nil {
		return err
	}
	if s.ID == 0 {
		// 首次：把 yaml 现值迁移入库
		return c.SaveDynamicToDB(db)
	}
	var d DynamicConfig
	if err := json.Unmarshal([]byte(s.Data), &d); err != nil {
		return err
	}
	c.applyDynamic(d)
	return nil
}

// SaveDynamicToDB 将当前动态配置写入数据库单行记录。
func (c *Config) SaveDynamicToDB(db *gorm.DB) error {
	data, err := json.Marshal(c.DynamicSnapshot())
	if err != nil {
		return err
	}
	return db.Save(&SettingStore{ID: 1, Data: string(data)}).Error
}
