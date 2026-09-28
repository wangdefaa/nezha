package tsdb

import (
	"strconv"
	"sync"
	"time"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/prompb"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/storage"
)

type bufferedWriter struct {
	db          *TSDB
	buffer      []storage.MetricRow
	mu          sync.Mutex
	maxSize     int
	flushTicker *time.Ticker
	stopCh      chan struct{}
	wg          sync.WaitGroup
}

func newBufferedWriter(db *TSDB, maxSize int, flushInterval time.Duration) *bufferedWriter {
	w := &bufferedWriter{
		db:          db,
		buffer:      make([]storage.MetricRow, 0, maxSize),
		maxSize:     maxSize,
		flushTicker: time.NewTicker(flushInterval),
		stopCh:      make(chan struct{}),
	}
	w.wg.Add(1)
	go w.flushLoop()
	return w
}

func (w *bufferedWriter) flushLoop() {
	defer w.wg.Done()
	for {
		select {
		case <-w.flushTicker.C:
			w.flush()
		case <-w.stopCh:
			w.flush()
			return
		}
	}
}

func (w *bufferedWriter) write(rows []storage.MetricRow) {
	if !w.db.acceptsWrites() {
		w.discard()
		return
	}

	w.mu.Lock()
	w.buffer = append(w.buffer, rows...)
	if len(w.buffer) >= w.maxSize {
		rows := w.buffer
		w.buffer = make([]storage.MetricRow, 0, w.maxSize)
		w.mu.Unlock()
		w.db.addRowsSafely(rows)
		return
	}
	w.mu.Unlock()
}

func (w *bufferedWriter) discard() {
	w.mu.Lock()
	w.buffer = make([]storage.MetricRow, 0, w.maxSize)
	w.mu.Unlock()
}

func (w *bufferedWriter) flush() {
	w.mu.Lock()
	if len(w.buffer) == 0 {
		w.mu.Unlock()
		return
	}
	rows := w.buffer
	w.buffer = make([]storage.MetricRow, 0, w.maxSize)
	w.mu.Unlock()

	w.db.addRowsSafely(rows)
}

func (w *bufferedWriter) stop() {
	w.flushTicker.Stop()
	close(w.stopCh)
	w.wg.Wait()
}

// MetricType 指标类型
type MetricType string

const (
	// 服务器指标
	MetricServerCPU            MetricType = "nezha_server_cpu"
	MetricServerMemory         MetricType = "nezha_server_memory"
	MetricServerSwap           MetricType = "nezha_server_swap"
	MetricServerDisk           MetricType = "nezha_server_disk"
	MetricServerNetInSpeed     MetricType = "nezha_server_net_in_speed"
	MetricServerNetOutSpeed    MetricType = "nezha_server_net_out_speed"
	MetricServerNetInTransfer  MetricType = "nezha_server_net_in_transfer"
	MetricServerNetOutTransfer MetricType = "nezha_server_net_out_transfer"
	MetricServerLoad1          MetricType = "nezha_server_load1"
	MetricServerLoad5          MetricType = "nezha_server_load5"
	MetricServerLoad15         MetricType = "nezha_server_load15"
	MetricServerTCPConn        MetricType = "nezha_server_tcp_conn"
	MetricServerUDPConn        MetricType = "nezha_server_udp_conn"
	MetricServerProcessCount   MetricType = "nezha_server_process_count"
	MetricServerUptime         MetricType = "nezha_server_uptime"

	// 服务监控指标
	MetricServiceDelay  MetricType = "nezha_service_delay"
	MetricServiceStatus MetricType = "nezha_service_status"
)

// ServerMetrics 服务器指标数据
type ServerMetrics struct {
	ServerID       uint64
	Timestamp      time.Time
	CPU            float64
	MemUsed        uint64
	SwapUsed       uint64
	DiskUsed       uint64
	NetInSpeed     uint64
	NetOutSpeed    uint64
	NetInTransfer  uint64
	NetOutTransfer uint64
	Load1          float64
	Load5          float64
	Load15         float64
	TCPConnCount   uint64
	UDPConnCount   uint64
	ProcessCount   uint64
	Uptime         uint64
}

// ServiceMetrics 服务监控指标数据
type ServiceMetrics struct {
	ServiceID  uint64
	ServerID   uint64
	Timestamp  time.Time
	Delay      float64
	Successful bool
}

func (db *TSDB) WriteServerMetrics(m *ServerMetrics) error {
	return db.writeRows(serverMetricRows(m))
}

func (db *TSDB) WriteServiceMetrics(m *ServiceMetrics) error {
	return db.writeRows(serviceMetricRows(m))
}

// serverMetricRows 把一条服务器指标展开成 15 个时序样本。
func serverMetricRows(m *ServerMetrics) []storage.MetricRow {
	ts := m.Timestamp.UnixMilli()
	id := strconv.FormatUint(m.ServerID, 10)
	return []storage.MetricRow{
		makeServerMetricRow(MetricServerCPU, id, ts, m.CPU),
		makeServerMetricRow(MetricServerMemory, id, ts, float64(m.MemUsed)),
		makeServerMetricRow(MetricServerSwap, id, ts, float64(m.SwapUsed)),
		makeServerMetricRow(MetricServerDisk, id, ts, float64(m.DiskUsed)),
		makeServerMetricRow(MetricServerNetInSpeed, id, ts, float64(m.NetInSpeed)),
		makeServerMetricRow(MetricServerNetOutSpeed, id, ts, float64(m.NetOutSpeed)),
		makeServerMetricRow(MetricServerNetInTransfer, id, ts, float64(m.NetInTransfer)),
		makeServerMetricRow(MetricServerNetOutTransfer, id, ts, float64(m.NetOutTransfer)),
		makeServerMetricRow(MetricServerLoad1, id, ts, m.Load1),
		makeServerMetricRow(MetricServerLoad5, id, ts, m.Load5),
		makeServerMetricRow(MetricServerLoad15, id, ts, m.Load15),
		makeServerMetricRow(MetricServerTCPConn, id, ts, float64(m.TCPConnCount)),
		makeServerMetricRow(MetricServerUDPConn, id, ts, float64(m.UDPConnCount)),
		makeServerMetricRow(MetricServerProcessCount, id, ts, float64(m.ProcessCount)),
		makeServerMetricRow(MetricServerUptime, id, ts, float64(m.Uptime)),
	}
}

// serviceMetricRows 把一次拨测结果展开成延迟与状态（成功=1）两个样本。
func serviceMetricRows(m *ServiceMetrics) []storage.MetricRow {
	ts := m.Timestamp.UnixMilli()
	serviceID := strconv.FormatUint(m.ServiceID, 10)
	serverID := strconv.FormatUint(m.ServerID, 10)
	var status float64
	if m.Successful {
		status = 1
	}
	return []storage.MetricRow{
		makeServiceMetricRow(MetricServiceDelay, serviceID, serverID, ts, m.Delay),
		makeServiceMetricRow(MetricServiceStatus, serviceID, serverID, ts, status),
	}
}

// writeRows 在读锁内写入样本：已关闭返回 errClosed；有缓冲写入器走缓冲，否则直写存储。
func (db *TSDB) writeRows(rows []storage.MetricRow) error {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return errClosed
	}
	if db.writer != nil {
		db.writer.write(rows)
	} else {
		db.addRowsSafely(rows)
	}
	return nil
}

func makeServerMetricRow(metric MetricType, serverID string, timestamp int64, value float64) storage.MetricRow {
	labels := []prompb.Label{
		{Name: "__name__", Value: string(metric)},
		{Name: "server_id", Value: serverID},
	}
	return storage.MetricRow{
		MetricNameRaw: storage.MarshalMetricNameRaw(nil, labels),
		Timestamp:     timestamp,
		Value:         value,
	}
}

func makeServiceMetricRow(metric MetricType, serviceID, serverID string, timestamp int64, value float64) storage.MetricRow {
	labels := []prompb.Label{
		{Name: "__name__", Value: string(metric)},
		{Name: "service_id", Value: serviceID},
		{Name: "server_id", Value: serverID},
	}
	return storage.MetricRow{
		MetricNameRaw: storage.MarshalMetricNameRaw(nil, labels),
		Timestamp:     timestamp,
		Value:         value,
	}
}

func (db *TSDB) WriteBatchServerMetrics(metrics []*ServerMetrics) error {
	rows := make([]storage.MetricRow, 0, len(metrics)*15)
	for _, m := range metrics {
		rows = append(rows, serverMetricRows(m)...)
	}
	return db.writeRows(rows)
}

func (db *TSDB) WriteBatchServiceMetrics(metrics []*ServiceMetrics) error {
	rows := make([]storage.MetricRow, 0, len(metrics)*2)
	for _, m := range metrics {
		rows = append(rows, serviceMetricRows(m)...)
	}
	return db.writeRows(rows)
}
