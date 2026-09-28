package singleton

import (
	"cmp"
	"slices"
	"sync"

	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/utils"
)

type ServerClass struct {
	class[uint64, *model.Server]

	// lifecycleMu 让权威 server 表的增删改与 ServiceSentinel 的同步上报处理串行化。
	lifecycleMu sync.RWMutex

	uuidToID map[string]uint64

	sortedListForGuest []*model.Server
}

func NewServerClass() *ServerClass {
	sc := &ServerClass{
		class: class[uint64, *model.Server]{
			list: make(map[uint64]*model.Server),
		},
		uuidToID: make(map[string]uint64),
	}

	var servers []model.Server
	DB.Find(&servers)
	for i := range servers {
		innerS := &servers[i]
		model.InitServer(innerS)
		sc.list[innerS.ID] = innerS
		sc.uuidToID[innerS.UUID] = innerS.ID
	}
	sc.sortList()

	model.OwnerServerIDsLookup = sc.ownerServerIDs
	model.AllServerIDsLookup = sc.allServerIDs
	model.OwnerIsAdminLookup = ownerIsAdmin

	return sc
}

func (c *ServerClass) ownerServerIDs(ownerUID uint64) []uint64 {
	var ids []uint64
	c.Range(func(id uint64, s *model.Server) bool {
		if s != nil && s.GetUserID() == ownerUID {
			ids = append(ids, id)
		}
		return true
	})
	return ids
}

func (c *ServerClass) allServerIDs() []uint64 {
	var ids []uint64
	c.Range(func(id uint64, s *model.Server) bool {
		if s != nil {
			ids = append(ids, id)
		}
		return true
	})
	return ids
}

func ownerIsAdmin(ownerUID uint64) bool {
	return userIsAdmin(ownerUID)
}

func (c *ServerClass) lockLifecycleRead()    { c.lifecycleMu.RLock() }
func (c *ServerClass) unlockLifecycleRead()  { c.lifecycleMu.RUnlock() }
func (c *ServerClass) lockLifecycleWrite()   { c.lifecycleMu.Lock() }
func (c *ServerClass) unlockLifecycleWrite() { c.lifecycleMu.Unlock() }

func (c *ServerClass) Update(s *model.Server, uuid string) {
	c.lockLifecycleWrite()
	defer c.unlockLifecycleWrite()

	c.listMu.Lock()

	c.list[s.ID] = s
	if uuid != "" {
		c.uuidToID[uuid] = s.ID
	}

	c.listMu.Unlock()

	c.sortList()
}

func (c *ServerClass) Delete(idList []uint64) {
	c.lockLifecycleWrite()
	defer c.unlockLifecycleWrite()

	c.listMu.Lock()

	for _, id := range idList {
		s, ok := c.list[id]
		if !ok {
			continue
		}
		delete(c.uuidToID, s.UUID)
		delete(c.list, id)
	}

	c.listMu.Unlock()

	c.sortList()
}

func (c *ServerClass) GetSortedListForGuest() []*model.Server {
	c.sortedListMu.RLock()
	defer c.sortedListMu.RUnlock()

	return slices.Clone(c.sortedListForGuest)
}

func (c *ServerClass) UUIDToID(uuid string) (id uint64, ok bool) {
	c.listMu.RLock()
	defer c.listMu.RUnlock()

	id, ok = c.uuidToID[uuid]
	return
}

func (c *ServerClass) sortList() {
	c.listMu.RLock()
	defer c.listMu.RUnlock()
	c.sortedListMu.Lock()
	defer c.sortedListMu.Unlock()

	c.sortedList = utils.MapValuesToSlice(c.list)
	// 按照服务器 ID 排序的具体实现（ID越大越靠前）
	slices.SortStableFunc(c.sortedList, func(a, b *model.Server) int {
		if a.DisplayIndex == b.DisplayIndex {
			return cmp.Compare(a.ID, b.ID)
		}
		return cmp.Compare(b.DisplayIndex, a.DisplayIndex)
	})

	c.sortedListForGuest = make([]*model.Server, 0, len(c.sortedList))
	for _, s := range c.sortedList {
		if !s.HideForGuest {
			c.sortedListForGuest = append(c.sortedListForGuest, s)
		}
	}
}

// DeleteServers 物理删除 server 及其分组成员、流量记录，并清理别处对它们的引用（见 refs.go）。
func DeleteServers(ids []uint64) error {
	var changes refChanges
	err := DB.Transaction(func(tx *gorm.DB) (err error) {
		changes, err = deleteServersTx(tx, ids)
		return err
	})
	if err != nil {
		return err
	}
	afterServersDeleted(ids, changes)
	return nil
}

// afterServersDeleted 提交后清理内存：告警周期统计、内存 server 表，再换上改写过引用的规则与拨测。
// 调用方不得持有 UserLock（ServerShared.Delete 取 lifecycleMu 写锁，见 OnUserDelete 的说明）。
func afterServersDeleted(ids []uint64, changes refChanges) {
	dropAlertTransferStats(ids)
	ServerShared.Delete(ids)
	changes.apply()
	pruneIPChangeSettings(ids, nil)
}

// deleteServersTx 在事务内删除 server 行及其分组成员、流量记录，并改写告警规则与拨测里的服务器范围。
func deleteServersTx(tx *gorm.DB, ids []uint64) (refChanges, error) {
	if err := tx.Unscoped().Delete(&model.Server{}, "id in (?)", ids).Error; err != nil {
		return refChanges{}, err
	}
	if err := tx.Unscoped().Delete(&model.ServerGroupServer{}, "server_id in (?)", ids).Error; err != nil {
		return refChanges{}, err
	}
	if err := tx.Unscoped().Delete(&model.Transfer{}, "server_id in (?)", ids).Error; err != nil {
		return refChanges{}, err
	}
	return pruneServerRefsTx(tx, ids)
}
