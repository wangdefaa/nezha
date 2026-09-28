package singleton

import (
	"errors"
	"log"
	"time"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/tsdb"
)

var TSDBShared *tsdb.TSDB

var openTSDB = tsdb.Open

func InitTSDB() error {
	config := tsdbConfigFromConf()
	if !config.Enabled() {
		log.Println("NEZHA>> TSDB is disabled (tsdb.data_path not configured)")
		if DB != nil {
			return DB.AutoMigrate(model.ServiceHistory{})
		}
		return nil
	}

	TSDBShared = nil
	db, err := openTSDB(config)
	if errors.Is(err, tsdb.ErrDiskFull) {
		fallbackWithoutTSDB(err)
		return nil
	}
	if err != nil {
		return err
	}
	TSDBShared = db
	log.Println("NEZHA>> TSDB initialized successfully")
	dropLegacyServiceHistories()
	return nil
}

// tsdbConfigFromConf 以默认值为底，叠加配置文件中的 tsdb 段。
func tsdbConfigFromConf() *tsdb.Config {
	config := &tsdb.Config{
		RetentionDays:      30,
		MinFreeDiskSpaceGB: 1,
		MaxMemoryMB:        256,
	}
	if Conf.TSDB.DataPath != "" {
		config.DataPath = Conf.TSDB.DataPath
	}
	if Conf.TSDB.RetentionDays > 0 {
		config.RetentionDays = Conf.TSDB.RetentionDays
	}
	if Conf.TSDB.MinFreeDiskSpaceGB > 0 {
		config.MinFreeDiskSpaceGB = Conf.TSDB.MinFreeDiskSpaceGB
	}
	if Conf.TSDB.MaxMemoryMB > 0 {
		config.MaxMemoryMB = Conf.TSDB.MaxMemoryMB
	}
	if Conf.TSDB.WriteBufferSize > 0 {
		config.WriteBufferSize = Conf.TSDB.WriteBufferSize
	}
	if Conf.TSDB.WriteBufferFlushInterval > 0 {
		config.WriteBufferFlushInterval = time.Duration(Conf.TSDB.WriteBufferFlushInterval) * time.Second
	}
	return config
}

// fallbackWithoutTSDB TSDB 所在磁盘已满时不阻断启动：降级为 service_histories 记录拨测历史。
func fallbackWithoutTSDB(err error) {
	log.Printf("NEZHA>> Warning: TSDB is unavailable because its disk is full; dashboard and alerts will continue without TSDB writes: %v", err)
	if DB == nil {
		return
	}
	if migrateErr := DB.AutoMigrate(model.ServiceHistory{}); migrateErr != nil {
		log.Printf("NEZHA>> Warning: failed to prepare SQLite service history fallback: %v", migrateErr)
	}
}

// dropLegacyServiceHistories 启用 TSDB 后删除旧的 service_histories 表（历史数据不迁移）。
func dropLegacyServiceHistories() {
	if DB == nil || !DB.Migrator().HasTable("service_histories") {
		return
	}
	log.Println("NEZHA>> Dropping legacy service_histories table (TSDB is now enabled). Historical data will NOT be migrated.")
	if err := DB.Migrator().DropTable("service_histories"); err != nil {
		log.Printf("NEZHA>> Warning: failed to drop service_histories table: %v", err)
	}
}

func TSDBEnabled() bool {
	return TSDBShared != nil && !TSDBShared.IsClosed()
}

func CloseTSDB() {
	if TSDBShared != nil {
		TSDBShared.Close()
	}
}
