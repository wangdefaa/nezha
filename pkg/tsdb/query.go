package tsdb

import (
	"fmt"
	"log"
	"sort"
	"strconv"
	"time"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/storage"

	"github.com/nezhahq/nezha/model"
)

// QueryPeriod 查询时间段
type QueryPeriod string

const (
	Period1Day   QueryPeriod = "1d"
	Period7Days  QueryPeriod = "7d"
	Period30Days QueryPeriod = "30d"
)

// ParseQueryPeriod 解析查询时间段
func ParseQueryPeriod(s string) (QueryPeriod, error) {
	switch s {
	case "1d", "":
		return Period1Day, nil
	case "7d":
		return Period7Days, nil
	case "30d":
		return Period30Days, nil
	default:
		return "", fmt.Errorf("invalid period: %s, expected 1d, 7d, or 30d", s)
	}
}

// Duration 返回时间段的时长
func (p QueryPeriod) Duration() time.Duration {
	switch p {
	case Period7Days:
		return 7 * 24 * time.Hour
	case Period30Days:
		return 30 * 24 * time.Hour
	default:
		return 24 * time.Hour
	}
}

// DownsampleInterval 返回降采样间隔
// 1d: 30秒一个点 (2880个点)
// 7d: 30分钟一个点 (336个点)
// 30d: 2小时一个点 (360个点)
func (p QueryPeriod) DownsampleInterval() time.Duration {
	switch p {
	case Period7Days:
		return 30 * time.Minute
	case Period30Days:
		return 2 * time.Hour
	default:
		return 30 * time.Second
	}
}

// Type aliases for model types used in tsdb package
type (
	DataPoint             = model.DataPoint
	ServiceHistorySummary = model.ServiceHistorySummary
	ServerServiceStats    = model.ServerServiceStats
	ServiceHistoryResult  = model.ServiceHistoryResponse
	MetricDataPoint       = model.ServerMetricsDataPoint
)

type rawDataPoint struct {
	timestamp int64
	value     float64
	status    float64
	hasDelay  bool
	hasStatus bool
}

func (db *TSDB) QueryServiceHistory(serviceID uint64, period QueryPeriod) (*ServiceHistoryResult, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return nil, errClosed
	}

	byServer, err := db.queryDelayStatus("service_id", serviceID, "server_id", recentRange(period))
	if err != nil {
		return nil, err
	}

	result := &ServiceHistoryResult{
		ServiceID: serviceID,
		Servers:   make([]ServerServiceStats, 0, len(byServer)),
	}
	for serverID, points := range byServer {
		result.Servers = append(result.Servers, ServerServiceStats{
			ServerID: serverID,
			Stats:    calculateStats(points, period.DownsampleInterval()),
		})
	}
	sort.Slice(result.Servers, func(i, j int) bool {
		return result.Servers[i].ServerID < result.Servers[j].ServerID
	})
	return result, nil
}

// recentRange 返回 [now-period, now] 的毫秒时间范围。
func recentRange(period QueryPeriod) storage.TimeRange {
	now := time.Now()
	return storage.TimeRange{
		MinTimestamp: now.Add(-period.Duration()).UnixMilli(),
		MaxTimestamp: now.UnixMilli(),
	}
}

// queryDelayStatus 查询 filterTag=filterID 的延迟与状态样本，按 groupTag 分组并按时间戳合并。
func (db *TSDB) queryDelayStatus(filterTag string, filterID uint64, groupTag string, tr storage.TimeRange) (map[uint64][]rawDataPoint, error) {
	idStr := strconv.FormatUint(filterID, 10)
	delayData, err := db.queryMetricGroupedBy(MetricServiceDelay, filterTag, idStr, groupTag, tr)
	if err != nil {
		return nil, fmt.Errorf("failed to query delay data: %w", err)
	}
	statusData, err := db.queryMetricGroupedBy(MetricServiceStatus, filterTag, idStr, groupTag, tr)
	if err != nil {
		return nil, fmt.Errorf("failed to query status data: %w", err)
	}
	return mergeDelayStatus(delayData, statusData), nil
}

// mergeDelayStatus 把同一分组、同一时间戳的延迟与状态样本合并成一个 rawDataPoint（结果无序）。
func mergeDelayStatus(delayData, statusData map[uint64][]metricPoint) map[uint64][]rawDataPoint {
	merged := make(map[uint64]map[int64]*rawDataPoint)
	pointAt := func(id uint64, ts int64) *rawDataPoint {
		if merged[id] == nil {
			merged[id] = make(map[int64]*rawDataPoint)
		}
		if merged[id][ts] == nil {
			merged[id][ts] = &rawDataPoint{timestamp: ts}
		}
		return merged[id][ts]
	}
	for id, points := range delayData {
		for _, p := range points {
			dp := pointAt(id, p.timestamp)
			dp.value, dp.hasDelay = p.value, true
		}
	}
	for id, points := range statusData {
		for _, p := range points {
			dp := pointAt(id, p.timestamp)
			dp.status, dp.hasStatus = p.value, true
		}
	}
	return flattenPoints(merged)
}

// flattenPoints 把「分组 → 时间戳 → 样本」展平为「分组 → 样本切片」（切片内无序）。
func flattenPoints(merged map[uint64]map[int64]*rawDataPoint) map[uint64][]rawDataPoint {
	out := make(map[uint64][]rawDataPoint, len(merged))
	for id, byTs := range merged {
		for _, p := range byTs {
			out[id] = append(out[id], *p)
		}
	}
	return out
}

type DailyServiceStats struct {
	Up    uint64
	Down  uint64
	Delay float64
}

// QueryServiceDailyStats 按天汇总 today（当天零点）之前 days 个整天的拨测结果，不含当天：
// 下标 days-1 是昨天，0 是 days 天前。
func (db *TSDB) QueryServiceDailyStats(serviceID uint64, today time.Time, days int) ([]DailyServiceStats, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return nil, errClosed
	}

	stats := make([]DailyServiceStats, days)
	serviceIDStr := strconv.FormatUint(serviceID, 10)

	start := today.AddDate(0, 0, -days)
	tr := storage.TimeRange{
		MinTimestamp: start.UnixMilli(),
		MaxTimestamp: today.UnixMilli(),
	}

	statusData, err := db.queryMetricGroupedBy(MetricServiceStatus, "service_id", serviceIDStr, "server_id", tr)
	if err != nil {
		return nil, err
	}
	delayData, err := db.queryMetricGroupedBy(MetricServiceDelay, "service_id", serviceIDStr, "server_id", tr)
	if err != nil {
		return nil, err
	}

	for _, points := range statusData {
		for _, p := range points {
			dayIndex, ok := dayIndexOf(today, p.timestamp, days)
			if !ok {
				continue
			}
			if p.value >= 0.5 {
				stats[dayIndex].Up++
			} else {
				stats[dayIndex].Down++
			}
		}
	}

	delayCount := make([]int, days)
	for _, points := range delayData {
		for _, p := range points {
			dayIndex, ok := dayIndexOf(today, p.timestamp, days)
			if !ok {
				continue
			}
			stats[dayIndex].Delay = (stats[dayIndex].Delay*float64(delayCount[dayIndex]) + p.value) / float64(delayCount[dayIndex]+1)
			delayCount[dayIndex]++
		}
	}

	return stats, nil
}

type metricPoint struct {
	timestamp int64
	value     float64
}

// dayIndexOf 把毫秒时间戳映射到 today 之前 days 天窗口的下标（days-1 为昨天，0 为最早一天）；越界返回 false。
func dayIndexOf(today time.Time, timestamp int64, days int) (int, bool) {
	idx := (days - 1) - int(today.Sub(time.UnixMilli(timestamp)).Hours())/24
	return idx, idx >= 0 && idx < days
}

// searchMetric 按 metric 名与一个标签过滤查询，把每个数据块在 tr 内的样本交给 visit；
// visit 收到的切片会被复用，需要保留时自行拷贝。
func (db *TSDB) searchMetric(metric MetricType, tagKey, tagValue string, tr storage.TimeRange,
	visit func(metricNameRaw []byte, timestamps []int64, values []float64)) error {
	tfs := storage.NewTagFilters()
	if err := tfs.Add(nil, []byte(metric), false, false); err != nil {
		return err
	}
	if err := tfs.Add([]byte(tagKey), []byte(tagValue), false, false); err != nil {
		return err
	}

	deadline := uint64(time.Now().Add(30 * time.Second).Unix())
	var search storage.Search
	search.Init(nil, db.storage, []*storage.TagFilters{tfs}, tr, 100000, deadline)
	defer search.MustClose()

	var timestamps []int64
	var values []float64
	for search.NextMetricBlock() {
		mbr := search.MetricBlockRef
		var block storage.Block
		mbr.BlockRef.MustReadBlock(&block)
		if err := block.UnmarshalData(); err != nil {
			log.Printf("NEZHA>> TSDB: failed to unmarshal block data: %v", err)
			continue
		}
		timestamps, values = block.AppendRowsWithTimeRangeFilter(timestamps[:0], values[:0], tr)
		visit(mbr.MetricName, timestamps, values)
	}
	return search.Error()
}

// queryMetricGroupedBy 查询 filterTag=filterValue 的 metric 样本，按 groupTag 的数值分组。
func (db *TSDB) queryMetricGroupedBy(metric MetricType, filterTag, filterValue, groupTag string, tr storage.TimeRange) (map[uint64][]metricPoint, error) {
	result := make(map[uint64][]metricPoint)
	err := db.searchMetric(metric, filterTag, filterValue, tr, func(nameRaw []byte, timestamps []int64, values []float64) {
		id, ok := tagUint(nameRaw, groupTag)
		if !ok {
			return
		}
		for i := range timestamps {
			result[id] = append(result[id], metricPoint{timestamp: timestamps[i], value: values[i]})
		}
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// tagUint 从原始 metric name 中解析 tag 的 uint64 值；缺失或非法时返回 false。
func tagUint(metricNameRaw []byte, tag string) (uint64, bool) {
	mn := storage.GetMetricName()
	defer storage.PutMetricName(mn)
	if err := mn.Unmarshal(metricNameRaw); err != nil {
		log.Printf("NEZHA>> TSDB: failed to unmarshal metric name: %v", err)
		return 0, false
	}
	raw := string(mn.GetTagValue(tag))
	if raw == "" {
		return 0, false
	}
	id, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		log.Printf("NEZHA>> TSDB: failed to parse %s %q: %v", tag, raw, err)
		return 0, false
	}
	return id, true
}

func calculateStats(points []rawDataPoint, downsampleInterval time.Duration) ServiceHistorySummary {
	if len(points) == 0 {
		return ServiceHistorySummary{}
	}

	sort.Slice(points, func(i, j int) bool {
		return points[i].timestamp < points[j].timestamp
	})

	var totalDelay float64
	var delayCount int
	var totalUp, totalDown uint64

	for _, p := range points {
		if p.hasDelay {
			totalDelay += p.value
			delayCount++
		}
		if p.hasStatus {
			if p.status >= 0.5 {
				totalUp++
			} else {
				totalDown++
			}
		}
	}

	summary := ServiceHistorySummary{
		TotalUp:   totalUp,
		TotalDown: totalDown,
	}

	if delayCount > 0 {
		summary.AvgDelay = totalDelay / float64(delayCount)
	}

	if totalUp+totalDown > 0 {
		summary.UpPercent = float32(totalUp) / float32(totalUp+totalDown) * 100
	}

	summary.DataPoints = downsample(points, downsampleInterval)

	return summary
}

func downsample(points []rawDataPoint, interval time.Duration) []DataPoint {
	if len(points) == 0 {
		return nil
	}

	intervalMs := interval.Milliseconds()
	result := make([]DataPoint, 0)

	// points 已排序，线性扫描分桶
	bucketStart := (points[0].timestamp / intervalMs) * intervalMs
	var totalDelay float64
	var delayCount, upCount, statusCount int

	flushBucket := func() {
		var avgDelay float64
		if delayCount > 0 {
			avgDelay = totalDelay / float64(delayCount)
		}
		var status uint8
		if statusCount > 0 && upCount > statusCount/2 {
			status = 1
		}
		result = append(result, DataPoint{
			Timestamp: bucketStart,
			Delay:     avgDelay,
			Status:    status,
		})
	}

	for _, p := range points {
		key := (p.timestamp / intervalMs) * intervalMs
		if key != bucketStart {
			flushBucket()
			bucketStart = key
			totalDelay = 0
			delayCount = 0
			upCount = 0
			statusCount = 0
		}
		if p.hasDelay {
			totalDelay += p.value
			delayCount++
		}
		if p.hasStatus {
			statusCount++
			if p.status >= 0.5 {
				upCount++
			}
		}
	}
	flushBucket()

	return result
}

func downsampleMetrics(points []rawDataPoint, interval time.Duration, useLastValue bool) []MetricDataPoint {
	if len(points) == 0 {
		return nil
	}

	sort.Slice(points, func(i, j int) bool {
		return points[i].timestamp < points[j].timestamp
	})

	intervalMs := interval.Milliseconds()
	result := make([]MetricDataPoint, 0)

	bucketStart := (points[0].timestamp / intervalMs) * intervalMs
	var total float64
	var count int
	var last rawDataPoint

	flushBucket := func() {
		var value float64
		if useLastValue {
			value = last.value
		} else if count > 0 {
			value = total / float64(count)
		}
		result = append(result, MetricDataPoint{
			Timestamp: bucketStart,
			Value:     value,
		})
	}

	for _, p := range points {
		key := (p.timestamp / intervalMs) * intervalMs
		if key != bucketStart {
			flushBucket()
			bucketStart = key
			total = 0
			count = 0
		}
		total += p.value
		count++
		last = p
	}
	flushBucket()

	return result
}

// isCumulativeMetric 判断指标是否为累积型（单调递增）
func isCumulativeMetric(metric MetricType) bool {
	switch metric {
	case MetricServerNetInTransfer, MetricServerNetOutTransfer, MetricServerUptime:
		return true
	default:
		return false
	}
}

func (db *TSDB) QueryServerMetrics(serverID uint64, metric MetricType, period QueryPeriod) ([]MetricDataPoint, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return nil, errClosed
	}

	var points []rawDataPoint
	serverIDStr := strconv.FormatUint(serverID, 10)
	err := db.searchMetric(metric, "server_id", serverIDStr, recentRange(period), func(_ []byte, timestamps []int64, values []float64) {
		for i := range timestamps {
			points = append(points, rawDataPoint{timestamp: timestamps[i], value: values[i]})
		}
	})
	if err != nil {
		return nil, err
	}
	return downsampleMetrics(points, period.DownsampleInterval(), isCumulativeMetric(metric)), nil
}

func (db *TSDB) QueryServiceHistoryByServerID(serverID uint64, period QueryPeriod) (map[uint64]*ServiceHistoryResult, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return nil, errClosed
	}

	byService, err := db.queryDelayStatus("server_id", serverID, "service_id", recentRange(period))
	if err != nil {
		return nil, err
	}

	results := make(map[uint64]*ServiceHistoryResult, len(byService))
	for serviceID, points := range byService {
		results[serviceID] = &ServiceHistoryResult{
			ServiceID: serviceID,
			Servers: []ServerServiceStats{{
				ServerID: serverID,
				Stats:    calculateStats(points, period.DownsampleInterval()),
			}},
		}
	}
	return results, nil
}
