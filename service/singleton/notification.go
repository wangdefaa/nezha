package singleton

import (
	"cmp"
	"fmt"
	"log"
	"slices"
	"sync"
	"time"

	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/utils"
)

const (
	firstNotificationDelay = time.Minute * 15
)

type NotificationClass struct {
	class[uint64, *model.Notification]

	groupToIDList map[uint64]map[uint64]*model.Notification
	idToGroupList map[uint64]map[uint64]struct{}

	groupList map[uint64]string
	groupMu   sync.RWMutex
}

func NewNotificationClass() *NotificationClass {
	var sortedList []*model.Notification

	groupToIDList := make(map[uint64]map[uint64]*model.Notification)
	idToGroupList := make(map[uint64]map[uint64]struct{})

	groupNotifications := make(map[uint64][]uint64)
	var ngn []model.NotificationGroupNotification
	DB.Find(&ngn)

	for _, n := range ngn {
		groupNotifications[n.NotificationGroupID] = append(groupNotifications[n.NotificationGroupID], n.NotificationID)
	}

	DB.Find(&sortedList)
	list := make(map[uint64]*model.Notification, len(sortedList))
	for _, n := range sortedList {
		list[n.ID] = n
	}

	var groups []model.NotificationGroup
	DB.Find(&groups)
	groupList := make(map[uint64]string)
	for _, grp := range groups {
		groupList[grp.ID] = grp.Name
	}

	for gid, nids := range groupNotifications {
		groupToIDList[gid] = make(map[uint64]*model.Notification)
		for _, nid := range nids {
			if n, ok := list[nid]; ok {
				groupToIDList[gid][n.ID] = n

				if idToGroupList[n.ID] == nil {
					idToGroupList[n.ID] = make(map[uint64]struct{})
				}

				idToGroupList[n.ID][gid] = struct{}{}
			}
		}
	}

	nc := &NotificationClass{
		class: class[uint64, *model.Notification]{
			list:       list,
			sortedList: sortedList,
		},
		groupToIDList: groupToIDList,
		idToGroupList: idToGroupList,
		groupList:     groupList,
	}
	return nc
}

func (c *NotificationClass) Update(n *model.Notification) {
	func() {
		c.listMu.Lock()
		defer c.listMu.Unlock()

		_, ok := c.list[n.ID]
		c.list[n.ID] = n
		if !ok {
			return
		}

		gids := c.idToGroupList[n.ID]
		for gid := range gids {
			group, exists := c.groupToIDList[gid]
			if !exists {
				delete(gids, gid)
				continue
			}
			group[n.ID] = n
		}
		if len(gids) == 0 {
			delete(c.idToGroupList, n.ID)
		}
	}()
	c.sortList()
}

func (c *NotificationClass) UpdateGroup(ng *model.NotificationGroup, ngn []uint64) {
	c.groupMu.Lock()
	defer c.groupMu.Unlock()

	c.groupList[ng.ID] = ng.Name

	c.listMu.Lock()
	defer c.listMu.Unlock()

	oldList := c.groupToIDList[ng.ID]
	newList := make(map[uint64]*model.Notification, len(ngn))
	for _, nid := range ngn {
		n, ok := c.list[nid]
		if !ok {
			continue
		}
		newList[nid] = n
		if c.idToGroupList[nid] == nil {
			c.idToGroupList[nid] = make(map[uint64]struct{})
		}
		c.idToGroupList[nid][ng.ID] = struct{}{}
	}
	c.unlinkRemovedMembers(ng.ID, oldList, newList)
	c.groupToIDList[ng.ID] = newList
}

// unlinkRemovedMembers 须持 listMu：把已移出通知组的通知从反向索引中摘除。
func (c *NotificationClass) unlinkRemovedMembers(gid uint64, oldList, newList map[uint64]*model.Notification) {
	for oldID := range oldList {
		if _, ok := newList[oldID]; ok {
			continue
		}
		delete(c.idToGroupList[oldID], gid)
		if len(c.idToGroupList[oldID]) == 0 {
			delete(c.idToGroupList, oldID)
		}
	}
}

func (c *NotificationClass) Delete(idList []uint64) {
	func() {
		c.listMu.Lock()
		defer c.listMu.Unlock()

		for _, id := range idList {
			delete(c.list, id)
			// 如果绑定了通知组才删除
			if gids, ok := c.idToGroupList[id]; ok {
				for gid := range gids {
					delete(c.groupToIDList[gid], id)
				}
				delete(c.idToGroupList, id)
			}
		}
	}()
	c.sortList()
}

// DeleteNotificationGroups 删除通知组及其成员，并把引用它们的告警规则、拨测与 IP 变更提醒改为不发送通知。
func DeleteNotificationGroups(ids []uint64) error {
	var changes refChanges
	err := DB.Transaction(func(tx *gorm.DB) (err error) {
		if err = tx.Unscoped().Delete(&model.NotificationGroup{}, "id in (?)", ids).Error; err != nil {
			return err
		}
		err = tx.Unscoped().Delete(&model.NotificationGroupNotification{}, "notification_group_id in (?)", ids).Error
		if err != nil {
			return err
		}
		changes, err = resetGroupRefsTx(tx, ids)
		return err
	})
	if err != nil {
		return err
	}
	NotificationShared.DeleteGroup(ids)
	changes.apply()
	pruneIPChangeSettings(nil, ids)
	return nil
}

func (c *NotificationClass) DeleteGroup(gids []uint64) {
	c.groupMu.Lock()
	defer c.groupMu.Unlock()
	c.listMu.Lock()
	defer c.listMu.Unlock()

	for _, gid := range gids {
		for nid := range c.groupToIDList[gid] {
			delete(c.idToGroupList[nid], gid)
			if len(c.idToGroupList[nid]) == 0 {
				delete(c.idToGroupList, nid)
			}
		}
		delete(c.groupList, gid)
		delete(c.groupToIDList, gid)
	}
}

func (c *NotificationClass) GetGroupName(gid uint64) string {
	c.groupMu.RLock()
	defer c.groupMu.RUnlock()

	return c.groupList[gid]
}

func (c *NotificationClass) sortList() {
	c.listMu.RLock()
	defer c.listMu.RUnlock()

	sortedList := utils.MapValuesToSlice(c.list)
	slices.SortFunc(sortedList, func(a, b *model.Notification) int {
		return cmp.Compare(a.ID, b.ID)
	})

	c.sortedListMu.Lock()
	defer c.sortedListMu.Unlock()
	c.sortedList = sortedList
}

func (c *NotificationClass) UnMuteNotification(notificationGroupID uint64, muteLabel string) {
	fullMuteLabel := NotificationMuteLabel.AppendNotificationGroupName(muteLabel, c.GetGroupName(notificationGroupID))
	Cache.Delete(fullMuteLabel)
}

// SendNotification 向指定的通知方式组的所有通知方式发送通知
func (c *NotificationClass) SendNotification(notificationGroupID uint64, desc string, muteLabel string, ext ...*model.Server) {
	if muteLabel != "" && !c.allowByMuteLabel(notificationGroupID, muteLabel, desc) {
		return
	}
	// 向该通知方式组的所有通知方式发出通知
	notifications := c.groupNotifications(notificationGroupID)
	for _, n := range notifications {
		log.Printf("NEZHA>> Try to notify %s", n.Name)
	}
	for _, n := range notifications {
		sendNotificationTo(n, desc, ext)
	}
}

// allowByMuteLabel 通知防骚扰：静音标签（附加通知组名）首次直接放行，之后每次提醒等待时间翻倍，最后每天最多一次。
func (c *NotificationClass) allowByMuteLabel(notificationGroupID uint64, muteLabel, desc string) bool {
	// 将通知方式组名称加入静音标志
	muteLabel = NotificationMuteLabel.AppendNotificationGroupName(muteLabel, c.GetGroupName(notificationGroupID))
	cacheN, has := Cache.Get(muteLabel)
	if !has {
		// 新提醒直接通知
		Cache.Set(muteLabel, NotificationHistory{
			Duration: firstNotificationDelay,
			Until:    time.Now().Add(firstNotificationDelay),
		}, firstNotificationDelay+time.Minute*10)
		return true
	}
	nHistory := cacheN.(NotificationHistory)
	if !time.Now().After(nHistory.Until) {
		if Conf.Debug {
			log.Println("NEZHA>> Muted repeated notification", desc, muteLabel)
		}
		return false
	}
	nHistory.Duration = min(nHistory.Duration*2, time.Hour*24)
	nHistory.Until = time.Now().Add(nHistory.Duration)
	// 缓存有效期加 10 分钟
	Cache.Set(muteLabel, nHistory, nHistory.Duration+time.Minute*10)
	return true
}

// groupNotifications 锁内拷贝通知组成员后立即释放：webhook 投递可能持续数分钟，不能阻塞通知与通知组的更新。
func (c *NotificationClass) groupNotifications(gid uint64) []*model.Notification {
	c.listMu.RLock()
	defer c.listMu.RUnlock()
	notifications := make([]*model.Notification, 0, len(c.groupToIDList[gid]))
	for _, n := range c.groupToIDList[gid] {
		notifications = append(notifications, n)
	}
	return notifications
}

// sendNotificationTo 向单个通知方式发送，ext 可附带触发告警的服务器。
func sendNotificationTo(n *model.Notification, desc string, ext []*model.Server) {
	ns := model.NotificationServerBundle{
		Notification: n,
		Server:       nil,
		Loc:          Loc,
	}
	if len(ext) > 0 {
		ns.Server = ext[0]
	}
	if err := ns.Send(desc); err != nil {
		log.Printf("NEZHA>> Sending notification to %s failed: %v", n.Name, err)
	} else {
		log.Printf("NEZHA>> Sending notification to %s succeeded", n.Name)
	}
}

type _NotificationMuteLabel struct{}

var NotificationMuteLabel _NotificationMuteLabel

func (_NotificationMuteLabel) IPChanged(serverId uint64) string {
	return fmt.Sprintf("bf::ic-%d", serverId)
}

// ServerIncident 告警静音标签。参数顺序以调用方为准（serverID 在前），标签字符串不变。
func (_NotificationMuteLabel) ServerIncident(serverID uint64, alertID uint64) string {
	return fmt.Sprintf("bf::sei-%d-%d", serverID, alertID)
}

func (_NotificationMuteLabel) ServerIncidentResolved(serverID uint64, alertID uint64) string {
	return fmt.Sprintf("bf::seir-%d-%d", serverID, alertID)
}

func (_NotificationMuteLabel) AppendNotificationGroupName(label string, notificationGroupName string) string {
	return fmt.Sprintf("%s:%s", label, notificationGroupName)
}

func (_NotificationMuteLabel) ServiceLatencyMin(serviceId uint64) string {
	return fmt.Sprintf("bf::sln-%d", serviceId)
}

func (_NotificationMuteLabel) ServiceLatencyMax(serviceId uint64) string {
	return fmt.Sprintf("bf::slm-%d", serviceId)
}

func (_NotificationMuteLabel) ServiceStateChanged(serviceId uint64) string {
	return fmt.Sprintf("bf::ssc-%d", serviceId)
}

func (_NotificationMuteLabel) ServiceTLS(serviceId uint64, extraInfo string) string {
	return fmt.Sprintf("bf::stls-%d-%s", serviceId, extraInfo)
}
