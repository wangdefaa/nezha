package singleton

import (
	_ "embed"
	"fmt"
	"io"
	"iter"
	"log"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/patrickmn/go-cache"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"sigs.k8s.io/yaml"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/utils"
)

var Version = "debug"

var (
	Cache             *cache.Cache
	DB                *gorm.DB
	Loc               *time.Location
	DashboardBootTime = uint64(time.Now().Unix())

	ServerShared          *ServerClass
	ServiceSentinelShared *ServiceSentinel
	NotificationShared    *NotificationClass
	CronShared            *CronClass
)

//go:embed frontend-templates.yaml
var frontendTemplatesYAML []byte

func InitTimezoneAndCache() error {
	var err error
	Loc, err = time.LoadLocation(Conf.Location)
	if err != nil {
		return err
	}

	Cache = cache.New(5*time.Minute, 10*time.Minute)
	return nil
}

// LoadSingleton 加载子服务并执行
func LoadSingleton(bus chan<- *model.Service) (err error) {
	initI18n() // 加载本地化服务
	initUser() // 加载用户ID绑定表
	NotificationShared = NewNotificationClass()
	ServerShared = NewServerClass()
	CronShared = NewCronClass()
	// 最后初始化 ServiceSentinel
	ServiceSentinelShared, err = NewServiceSentinel(bus)
	return
}

// InitFrontendTemplates 从内置文件加载 builtinTemplates（启动期兜底校验 + 首启 seed 源）。
// 运行期权威清单由 themes 表经 ReloadThemes 提供，见 theme.go。
func InitFrontendTemplates() error {
	return yaml.Unmarshal(frontendTemplatesYAML, &builtinTemplates)
}

// InitDBFromPath 按配置初始化数据库；sqlite 时 path 作为文件路径回退。
func InitDBFromPath(path string) error {
	if err := initThemeDir(path); err != nil {
		return err
	}
	dialector, err := dbDialector(path)
	if err != nil {
		return err
	}
	DB, err = gorm.Open(dialector, &gorm.Config{CreateBatchSize: 200, Logger: gormLogger(os.Stdout)})
	if err != nil {
		return err
	}
	if Conf.Debug {
		DB = DB.Debug()
	}
	if err := migrateAndSeed(); err != nil {
		return err
	}
	if err := Conf.LoadDynamicFromDB(DB); err != nil {
		return err
	}
	return ReconcileTemplateSelection()
}

// gormLogger 同 logger.Default，但忽略 ErrRecordNotFound：否则未认证登录的任意长度用户名会随 SQL
// 原样打进日志（一次 64MiB 请求即写 64MiB 日志，且可伪造换行注入日志）。
func gormLogger(w io.Writer) logger.Interface {
	return logger.New(log.New(w, "\r\n", log.LstdFlags), logger.Config{
		SlowThreshold:             200 * time.Millisecond,
		LogLevel:                  logger.Warn,
		IgnoreRecordNotFoundError: true,
		Colorful:                  true,
	})
}

// initThemeDir 初始化自定义主题磁盘根目录（与数据库文件同级的 themes/）。
func initThemeDir(dbPath string) error {
	ThemeDir = filepath.Join(filepath.Dir(dbPath), "themes")
	return os.MkdirAll(ThemeDir, 0o750)
}

// migrateAndSeed 迁移表结构、登记内置主题并加载运行期主题清单。
func migrateAndSeed() error {
	if err := autoMigrate(); err != nil {
		return err
	}
	if err := SeedBuiltinThemes(DB); err != nil {
		return err
	}
	return ReloadThemes(DB)
}

// dbDialector 按 Conf.Database.Type 选择驱动（sqlite/mysql/postgres）。
func dbDialector(fallbackSqlitePath string) (gorm.Dialector, error) {
	dsn := Conf.Database.DSN
	switch Conf.Database.Type {
	case "mysql":
		return mysql.Open(dsn), nil
	case "postgres", "postgresql":
		return postgres.Open(dsn), nil
	case "", "sqlite", "sqlite3":
		if dsn == "" {
			dsn = fallbackSqlitePath
		}
		return sqlite.Open(dsn), nil
	default:
		return nil, fmt.Errorf("unsupported database type: %s", Conf.Database.Type)
	}
}

// autoMigrate 同步所有表结构。
func autoMigrate() error {
	return DB.AutoMigrate(model.Server{}, model.User{}, model.ServerGroup{}, model.NotificationGroup{},
		model.Notification{}, model.AlertRule{}, model.Service{}, model.NotificationGroupNotification{},
		model.Transfer{}, model.ServerGroupServer{},
		model.WAF{}, model.Oauth2Bind{}, model.JWTSession{},
		model.APIToken{}, model.SettingStore{}, model.Theme{})
}

// RecordTransferHourlyUsage 对流量记录进行打点
func RecordTransferHourlyUsage(servers ...*model.Server) {
	now := time.Now()
	nowTrimSeconds := time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), 0, 0, 0, now.Location())

	var txs []model.Transfer
	var slist iter.Seq[*model.Server]
	if len(servers) > 0 {
		slist = slices.Values(servers)
	} else {
		slist = utils.Seq2To1(ServerShared.Range)
	}

	for server := range slist {
		tx := model.Transfer{
			ServerID: server.ID,
			In:       utils.SubUintChecked(server.State.NetInTransfer, server.PrevTransferInSnapshot),
			Out:      utils.SubUintChecked(server.State.NetOutTransfer, server.PrevTransferOutSnapshot),
		}
		if tx.In == 0 && tx.Out == 0 {
			continue
		}
		server.PrevTransferInSnapshot = server.State.NetInTransfer
		server.PrevTransferOutSnapshot = server.State.NetOutTransfer
		tx.CreatedAt = nowTrimSeconds
		txs = append(txs, tx)
	}

	if len(txs) == 0 {
		return
	}
	log.Printf("NEZHA>> Saved traffic metrics to database. Affected %d row(s), Error: %v", len(txs), DB.Create(txs).Error)
}

// transferKeep 周期流量规则要求保留的数据起点：all 为 cover-all 规则的全局起点，special 为指定机器的起点。
type transferKeep struct {
	all        time.Time
	special    map[uint64]time.Time
	specialIDs []uint64
}

// transferBeforeCond 跨库「早于」条件；sqlite 以文本存时间，需 datetime() 规范化时区（同 model.transferTimeCond）。
func transferBeforeCond() string {
	if DB.Dialector.Name() == "sqlite" {
		return "datetime(created_at) < datetime(?)"
	}
	return "created_at < ?"
}

// CleanMonitorHistory 清理流量记录（TSDB 有自己的保留策略）
func CleanMonitorHistory() {
	// 清理已被删除的服务器的流量记录
	DB.Unscoped().Delete(&model.Transfer{}, "server_id NOT IN (SELECT id FROM servers)")
	var alerts []model.AlertRule
	// 规则读取失败时不能当作「无规则」处理，否则会把周期流量历史整表删光
	if err := DB.Find(&alerts).Error; err != nil {
		log.Printf("NEZHA>> Failed to load alert rules while cleaning transfer history: %v", err)
		return
	}
	deleteExpiredTransfers(collectTransferKeep(alerts))
}

// collectTransferKeep 计算各周期流量规则要求保留的最早时间点。
func collectTransferKeep(alerts []model.AlertRule) transferKeep {
	keep := transferKeep{special: make(map[uint64]time.Time)}
	for _, alert := range alerts {
		for _, rule := range alert.Rules {
			// 非周期规则或持久化数据非法（CycleStart 为空/CycleInterval 溢出）一律跳过，防启动期 panic/死循环
			if !rule.HasSafeCycleConfiguration() {
				continue
			}
			before := rule.GetTransferDurationStart().UTC()
			if rule.Cover == model.RuleCoverAll {
				if keep.all.IsZero() || keep.all.After(before) {
					keep.all = before
				}
				continue
			}
			for id := range rule.Ignore {
				if keep.special[id].IsZero() || keep.special[id].After(before) {
					keep.special[id] = before
					keep.specialIDs = append(keep.specialIDs, id)
				}
			}
		}
	}
	return keep
}

// deleteExpiredTransfers 按保留点删除流量记录；specialIDs 为空时不能用 NOT IN (?)（会渲染成 NOT IN (NULL)，一行都删不掉）。
func deleteExpiredTransfers(keep transferKeep) {
	for id, couldRemove := range keep.special {
		DB.Unscoped().Delete(&model.Transfer{}, "server_id = ? AND "+transferBeforeCond(), id, couldRemove)
	}
	if len(keep.specialIDs) == 0 {
		if keep.all.IsZero() {
			DB.Unscoped().Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(&model.Transfer{})
		} else {
			DB.Unscoped().Delete(&model.Transfer{}, transferBeforeCond(), keep.all)
		}
		return
	}
	if keep.all.IsZero() {
		DB.Unscoped().Delete(&model.Transfer{}, "server_id NOT IN (?)", keep.specialIDs)
	} else {
		DB.Unscoped().Delete(&model.Transfer{}, "server_id NOT IN (?) AND "+transferBeforeCond(), keep.specialIDs, keep.all)
	}
}

// PerformMaintenance 执行系统维护（SQLite VACUUM 和 TSDB 维护）
func PerformMaintenance() {
	log.Println("NEZHA>> Starting system maintenance...")

	// 1. SQLite 维护（仅 sqlite 需要 VACUUM；mysql/postgres 自带回收机制）
	if DB != nil && DB.Dialector.Name() == "sqlite" {
		log.Println("NEZHA>> SQLite: Starting VACUUM...")
		if err := DB.Exec("VACUUM").Error; err != nil {
			log.Printf("NEZHA>> SQLite: VACUUM failed: %v", err)
		} else {
			log.Println("NEZHA>> SQLite: VACUUM completed")
		}
	}

	// 2. TSDB 维护
	if TSDBEnabled() {
		TSDBShared.Maintenance()
	}

	log.Println("NEZHA>> System maintenance completed")
}

// IPDesensitize 根据设置选择是否对IP进行打码处理 返回处理后的IP(关闭打码则返回原IP)
func IPDesensitize(ip string) string {
	if Conf.EnablePlainIPInNotification {
		return ip
	}
	return utils.IPDesensitize(ip)
}

type class[K comparable, V model.CommonInterface] struct {
	list   map[K]V
	listMu sync.RWMutex

	sortedList   []V
	sortedListMu sync.RWMutex
}

func (c *class[K, V]) Get(id K) (s V, ok bool) {
	c.listMu.RLock()
	defer c.listMu.RUnlock()

	s, ok = c.list[id]
	return
}

func (c *class[K, V]) GetList() map[K]V {
	c.listMu.RLock()
	defer c.listMu.RUnlock()

	return maps.Clone(c.list)
}

func (c *class[K, V]) GetSortedList() []V {
	c.sortedListMu.RLock()
	defer c.sortedListMu.RUnlock()

	return slices.Clone(c.sortedList)
}

func (c *class[K, V]) Range(fn func(k K, v V) bool) {
	c.listMu.RLock()
	defer c.listMu.RUnlock()

	for k, v := range c.list {
		if !fn(k, v) {
			break
		}
	}
}

func (c *class[K, V]) CheckPermission(ctx *gin.Context, idList iter.Seq[K]) bool {
	c.listMu.RLock()
	defer c.listMu.RUnlock()

	for id := range idList {
		if s, ok := c.list[id]; ok {
			if !s.HasPermission(ctx) {
				return false
			}
		}
	}
	return true
}
