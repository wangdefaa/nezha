package singleton

import (
	"log"
	"strconv"
	"strings"

	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/utils"
)

var Conf *ConfigClass

type ConfigClass struct {
	*model.Config

	IgnoredIPNotificationServerIDs map[uint64]bool `json:"ignored_ip_notification_server_ids,omitempty"`
	Oauth2Providers                []string        `json:"oauth2_providers,omitempty"`
}

// InitConfigFromPath 从给出的文件路径中加载配置
func InitConfigFromPath(path string) error {
	Conf = &ConfigClass{
		Config: &model.Config{},
	}
	err := Conf.Read(path, builtinTemplates)
	if err != nil {
		return err
	}
	rotated, err := Conf.RotateJWTSecretKeyIfNeeded(Version)
	if err != nil {
		return err
	}
	if rotated {
		log.Printf("NEZHA>> Rotated jwt_secret_key for dashboard version %s", Version)
	}

	Conf.refreshDerived()
	return nil
}

// updateIgnoredIPNotificationID 按逗号分隔的服务器 ID 重建 IP 变更通知的覆盖集合。
// 先建好新 map 再整体替换：列表清空时旧集合也必须清掉（原实现提前 return，清空后仍按旧列表生效）；
// ID 两侧的空格要去掉，否则「1, 2」里的 2 会被静默丢弃。
func (c *ConfigClass) updateIgnoredIPNotificationID() {
	ids := make(map[uint64]bool)
	for raw := range strings.SplitSeq(c.IgnoredIPNotification, ",") {
		if id, _ := strconv.ParseUint(strings.TrimSpace(raw), 10, 64); id > 0 {
			ids[id] = true
		}
	}
	c.IgnoredIPNotificationServerIDs = ids
}

// refreshDerived 刷新派生缓存：oauth2 名单与忽略IP集合。
func (c *ConfigClass) refreshDerived() {
	c.Oauth2Providers = utils.MapKeysToSlice(c.Oauth2)
	c.updateIgnoredIPNotificationID()
}

// LoadDynamicFromDB 加载动态配置后刷新派生缓存（覆盖嵌入方法）。
func (c *ConfigClass) LoadDynamicFromDB(db *gorm.DB) error {
	if err := c.Config.LoadDynamicFromDB(db); err != nil {
		return err
	}
	c.refreshDerived()
	return nil
}

// SaveDynamicToDB 持久化动态配置前刷新派生缓存（覆盖嵌入方法）。
func (c *ConfigClass) SaveDynamicToDB(db *gorm.DB) error {
	c.refreshDerived()
	return c.Config.SaveDynamicToDB(db)
}
