package controller

import (
	"errors"
	"slices"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/copier"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/tsdb"
	pb "github.com/nezhahq/nezha/proto"
	"github.com/nezhahq/nezha/service/singleton"
)

// List server
// @Summary List server
// @Security BearerAuth
// @Security APITokenAuth
// @Schemes
// @Description List server. PAT scope required: nezha:inventory:read.
// @Tags auth required
// @Param id query uint false "Resource ID"
// @Produce json
// @Success 200 {object} model.CommonResponse[[]model.Server]
// @Router /server [get]
func listServer(c *gin.Context) ([]*model.Server, error) {
	slist := singleton.ServerShared.GetSortedList()

	var ssl []*model.Server
	if err := copier.Copy(&ssl, &slist); err != nil {
		return nil, err
	}
	return ssl, nil
}

// Edit server
// @Summary Edit server
// @Security BearerAuth
// @Schemes
// @Description Edit server
// @Tags auth required
// @Accept json
// @Param id path uint true "Server ID"
// @Param body body model.ServerForm true "ServerForm"
// @Produce json
// @Success 200 {object} model.CommonResponse[any]
// @Router /server/{id} [patch]
func updateServer(c *gin.Context) (any, error) {
	idStr := c.Param("id")
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		return nil, err
	}
	var sf model.ServerForm
	if err := c.ShouldBindJSON(&sf); err != nil {
		return nil, err
	}

	var s model.Server
	if err := singleton.DB.First(&s, id).Error; err != nil {
		return nil, singleton.Localizer.ErrorT("server id %d does not exist", id)
	}

	if !s.HasPermission(c) {
		return nil, singleton.Localizer.ErrorT("permission denied")
	}

	s.Name = sf.Name
	s.DisplayIndex = sf.DisplayIndex
	s.Note = sf.Note
	s.PublicNote = sf.PublicNote
	s.HideForGuest = sf.HideForGuest

	if err := singleton.DB.Save(&s).Error; err != nil {
		return nil, newGormError("%v", err)
	}

	rs, _ := singleton.ServerShared.Get(s.ID)
	s.CopyFromRunningServer(rs)
	singleton.ServerShared.Update(&s, "")

	return nil, nil
}

// Batch delete server
// @Summary Batch delete server
// @Security BearerAuth
// @Schemes
// @Description Batch delete server
// @Tags auth required
// @Accept json
// @param request body []uint64 true "id list"
// @Produce json
// @Success 200 {object} model.CommonResponse[any]
// @Router /batch-delete/server [post]
func batchDeleteServer(c *gin.Context) (any, error) {
	var servers []uint64
	if err := c.ShouldBindJSON(&servers); err != nil {
		return nil, err
	}

	if !singleton.ServerShared.CheckPermission(c, slices.Values(servers)) {
		return nil, singleton.Localizer.ErrorT("permission denied")
	}

	err := singleton.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Unscoped().Delete(&model.Server{}, "id in (?)", servers).Error; err != nil {
			return err
		}
		if err := tx.Unscoped().Delete(&model.ServerGroupServer{}, "server_id in (?)", servers).Error; err != nil {
			return err
		}
		return nil
	})

	if err != nil {
		return nil, newGormError("%v", err)
	}

	singleton.AlertsLock.Lock()
	for _, sid := range servers {
		for _, alert := range singleton.Alerts {
			if singleton.AlertsCycleTransferStatsStore[alert.ID] != nil {
				delete(singleton.AlertsCycleTransferStatsStore[alert.ID].ServerName, sid)
				delete(singleton.AlertsCycleTransferStatsStore[alert.ID].Transfer, sid)
				delete(singleton.AlertsCycleTransferStatsStore[alert.ID].NextUpdate, sid)
			}
		}
	}
	singleton.DB.Unscoped().Delete(&model.Transfer{}, "server_id in (?)", servers)
	singleton.AlertsLock.Unlock()

	singleton.ServerShared.Delete(servers)
	return nil, nil
}

// Force update Agent
// @Summary Force update Agent
// @Security BearerAuth
// @Schemes
// @Description Force update Agent
// @Tags auth required
// @Accept json
// @param request body []uint64 true "id list"
// @Produce json
// @Success 200 {object} model.CommonResponse[model.ServerTaskResponse]
// @Router /force-update/server [post]
func forceUpdateServer(c *gin.Context) (*model.ServerTaskResponse, error) {
	var forceUpdateServers []uint64
	if err := c.ShouldBindJSON(&forceUpdateServers); err != nil {
		return nil, err
	}

	forceUpdateResp := new(model.ServerTaskResponse)

	for _, sid := range forceUpdateServers {
		server, _ := singleton.ServerShared.Get(sid)
		// Per-ID ownership check. Foreign servers (online or offline) and
		// unknown IDs MUST be indistinguishable in the response — otherwise the
		// response shape leaks server-ID existence/online-state, letting a
		// RoleMember enumerate other users' machines. We drop them into the
		// Offline bucket without actually dispatching the upgrade task.
		if server == nil || !server.HasPermission(c) {
			forceUpdateResp.Offline = append(forceUpdateResp.Offline, sid)
			continue
		}
		if server.GetTaskStream() != nil {
			if err := server.SendTask(&pb.Task{
				Type: model.TaskTypeUpgrade,
			}); err != nil {
				if errors.Is(err, model.ErrTaskStreamOffline) {
					forceUpdateResp.Offline = append(forceUpdateResp.Offline, sid)
				} else {
					forceUpdateResp.Failure = append(forceUpdateResp.Failure, sid)
				}
			} else {
				forceUpdateResp.Success = append(forceUpdateResp.Success, sid)
			}
		} else {
			forceUpdateResp.Offline = append(forceUpdateResp.Offline, sid)
		}
	}

	return forceUpdateResp, nil
}

var serverMetricMap = map[string]tsdb.MetricType{
	"cpu":              tsdb.MetricServerCPU,
	"memory":           tsdb.MetricServerMemory,
	"swap":             tsdb.MetricServerSwap,
	"disk":             tsdb.MetricServerDisk,
	"net_in_speed":     tsdb.MetricServerNetInSpeed,
	"net_out_speed":    tsdb.MetricServerNetOutSpeed,
	"net_in_transfer":  tsdb.MetricServerNetInTransfer,
	"net_out_transfer": tsdb.MetricServerNetOutTransfer,
	"load1":            tsdb.MetricServerLoad1,
	"load5":            tsdb.MetricServerLoad5,
	"load15":           tsdb.MetricServerLoad15,
	"tcp_conn":         tsdb.MetricServerTCPConn,
	"udp_conn":         tsdb.MetricServerUDPConn,
	"process_count":    tsdb.MetricServerProcessCount,
	"uptime":           tsdb.MetricServerUptime,
}

// Get server metrics history
// @Summary Get server metrics history
// @Security BearerAuth
// @Schemes
// @Description Get server metrics history for a specific server
// @Tags common
// @param id path uint true "Server ID"
// @param metric query string true "Metric name: cpu, memory, swap, disk, net_in_speed, net_out_speed, net_in_transfer, net_out_transfer, load1, load5, load15, tcp_conn, udp_conn, process_count, uptime"
// @param period query string false "Time period: 1d, 7d, 30d (default: 1d)"
// @Produce json
// @Success 200 {object} model.CommonResponse[model.ServerMetricsResponse]
// @Router /server/{id}/metrics [get]
func getServerMetrics(c *gin.Context) (*model.ServerMetricsResponse, error) {
	idStr := c.Param("id")
	serverID, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		return nil, err
	}

	server, ok := singleton.ServerShared.Get(serverID)
	if !ok {
		return nil, singleton.Localizer.ErrorT("server not found")
	}

	if !userCanViewServer(c, server) {
		return nil, singleton.Localizer.ErrorT("unauthorized")
	}
	_, isMember := c.Get(model.CtxKeyAuthorizedUser)

	metricName := c.Query("metric")
	metricType, ok := serverMetricMap[metricName]
	if !ok {
		return nil, singleton.Localizer.ErrorT("invalid metric name")
	}

	periodStr := c.DefaultQuery("period", "1d")
	period, err := tsdb.ParseQueryPeriod(periodStr)
	if err != nil {
		return nil, err
	}

	if !isMember && period != tsdb.Period1Day {
		return nil, singleton.Localizer.ErrorT("unauthorized: only 1d data available for guests")
	}

	response := &model.ServerMetricsResponse{
		ServerID:   serverID,
		ServerName: server.Name,
		Metric:     metricName,
		DataPoints: make([]model.ServerMetricsDataPoint, 0),
	}

	if !singleton.TSDBEnabled() {
		return response, nil
	}

	points, err := singleton.TSDBShared.QueryServerMetrics(serverID, metricType, period)
	if err != nil {
		return nil, err
	}

	response.DataPoints = points

	return response, nil
}
