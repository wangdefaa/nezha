package singleton

import (
	"log"
	"slices"
	"strconv"
	"strings"

	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
)

// 删除服务器或通知组后，别处还存着它们的 ID：告警规则与拨测的服务器范围和通知组，以及 IP 变更提醒的设置。
// 这些引用在删除所在的事务里一并改写：服务器范围去掉这些 ID，通知组改为 0（不发送通知）；提交后再换上内存副本。
// API 令牌的服务器白名单不动：白名单为空表示不限制，去掉最后一个 ID 反而会放大令牌的权限。

// refChanges 事务里改写过的告警规则与拨测，提交后用来替换内存副本。
type refChanges struct {
	rules    []*model.AlertRule
	services []*model.Service
}

func (c *refChanges) add(o refChanges) {
	c.rules = append(c.rules, o.rules...)
	c.services = append(c.services, o.services...)
}

// apply 换上内存副本；同一对象出现多次时以后出现的为准。
// 调用方不得持有 UserLock：告警与拨测协程持着各自的锁时会去取 UserLock。
func (c refChanges) apply() {
	replaceAlertRules(c.rules)
	if len(c.services) > 0 {
		ServiceSentinelShared.replaceServices(c.services)
	}
}

// pruneServerRefsTx 从告警规则与拨测的服务器范围里去掉 ids。范围的语义不变：这些服务器已经不存在。
func pruneServerRefsTx(tx *gorm.DB, ids []uint64) (refChanges, error) {
	rules, err := rewriteRows(tx, func(r *model.AlertRule) bool {
		changed := false
		for _, rule := range r.Rules {
			changed = deleteKeys(rule.Ignore, ids) || changed
		}
		return changed
	})
	if err != nil {
		return refChanges{}, err
	}
	services, err := rewriteRows(tx, func(s *model.Service) bool { return deleteKeys(s.SkipServers, ids) })
	return refChanges{rules: rules, services: services}, err
}

// resetGroupRefsTx 把指向 ids 的告警规则与拨测的通知组改为 0。
func resetGroupRefsTx(tx *gorm.DB, ids []uint64) (refChanges, error) {
	rules, err := rewriteRows(tx, func(r *model.AlertRule) bool { return resetID(&r.NotificationGroupID, ids) })
	if err != nil {
		return refChanges{}, err
	}
	services, err := rewriteRows(tx, func(s *model.Service) bool { return resetID(&s.NotificationGroupID, ids) })
	return refChanges{rules: rules, services: services}, err
}

// rewriteRows 对表里每一行调用 edit，保存并返回 edit 报告有改动的行。
func rewriteRows[T any](tx *gorm.DB, edit func(*T) bool) ([]*T, error) {
	var rows []*T
	if err := tx.Find(&rows).Error; err != nil {
		return nil, err
	}
	var changed []*T
	for _, row := range rows {
		if !edit(row) {
			continue
		}
		if err := tx.Save(row).Error; err != nil {
			return nil, err
		}
		changed = append(changed, row)
	}
	return changed, nil
}

// deleteKeys 从 m 里删掉 ids，返回是否删掉了什么。
func deleteKeys(m map[uint64]bool, ids []uint64) bool {
	n := len(m)
	for _, id := range ids {
		delete(m, id)
	}
	return len(m) != n
}

// resetID 在 *id 属于 ids 时把它清零，返回是否清零。
func resetID(id *uint64, ids []uint64) bool {
	if *id == 0 || !slices.Contains(ids, *id) {
		return false
	}
	*id = 0
	return true
}

// pruneIPChangeSettings 从 IP 变更提醒里去掉已删除的服务器（servers）与通知组（groups），有改动才写库。
// 删除本身已经提交，这一步失败只记日志，最坏情况是留下无效引用。
func pruneIPChangeSettings(servers, groups []uint64) {
	list, listChanged := dropIDsFromCSV(Conf.IgnoredIPNotification, servers)
	groupChanged := resetID(&Conf.IPChangeNotificationGroupID, groups)
	if !listChanged && !groupChanged {
		return
	}
	Conf.IgnoredIPNotification = list
	if err := Conf.SaveDynamicToDB(DB); err != nil {
		log.Printf("NEZHA>> 清理 IP 变更提醒设置里的无效引用失败：%v", err)
	}
}

// dropIDsFromCSV 从逗号分隔的 ID 列表里去掉 ids；有改动时顺带去掉其余片段两侧的空格。
func dropIDsFromCSV(csv string, ids []uint64) (string, bool) {
	parts := strings.Split(csv, ",")
	kept := slices.DeleteFunc(slices.Clone(parts), func(p string) bool {
		id, err := strconv.ParseUint(strings.TrimSpace(p), 10, 64)
		return err == nil && slices.Contains(ids, id)
	})
	if len(kept) == len(parts) {
		return csv, false
	}
	for i := range kept {
		kept[i] = strings.TrimSpace(kept[i])
	}
	return strings.Join(kept, ","), true
}
