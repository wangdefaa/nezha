package controller

import (
	"net"
	"slices"

	"github.com/gin-gonic/gin"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/utils"
	"github.com/nezhahq/nezha/service/singleton"
)

// List blocked addresses
// @Summary List blocked addresses
// @Security BearerAuth
// @Schemes
// @Description List blocked addresses
// @Tags admin required
// @Param limit query uint false "Page limit"
// @Param offset query uint false "Page offset"
// @Produce json
// @Success 200 {object} model.PaginatedResponse[[]model.WAFApiMock, model.WAFApiMock]
// @Router /waf [get]
func listBlockedAddress(c *gin.Context) (*model.Value[[]*model.WAFApiMock], error) {
	limit, offset := parsePagination(c)

	var waf []*model.WAF
	if err := singleton.DB.Order("block_timestamp DESC").Limit(limit).Offset(offset).Find(&waf).Error; err != nil {
		return nil, err
	}

	var total int64
	if err := singleton.DB.Model(&model.WAF{}).Count(&total).Error; err != nil {
		return nil, err
	}

	return &model.Value[[]*model.WAFApiMock]{
		Value: slices.Collect(utils.ConvertSeq(slices.Values(waf), func(e *model.WAF) *model.WAFApiMock {
			return &model.WAFApiMock{
				IP:              net.IP(e.IP).String(),
				BlockIdentifier: e.BlockIdentifier,
				BlockReason:     e.BlockReason,
				BlockTimestamp:  e.BlockTimestamp,
				Count:           e.Count,
			}
		})),
		Pagination: model.Pagination{
			Offset: offset,
			Limit:  limit,
			Total:  total,
		},
	}, nil
}

// Batch delete blocked addresses
// @Summary Batch delete blocked addresses
// @Security BearerAuth
// @Schemes
// @Description Batch delete blocked addresses
// @Tags admin required
// @Accept json
// @Param request body []string true "block list"
// @Produce json
// @Success 200 {object} model.CommonResponse[any]
// @Router /batch-delete/waf [post]
func batchDeleteBlockedAddress(c *gin.Context) (any, error) {
	var list []string
	if err := c.ShouldBindJSON(&list); err != nil {
		return nil, err
	}

	if err := model.BatchUnblockIP(singleton.DB, utils.Unique(list)); err != nil {
		return nil, newGormError("%v", err)
	}

	return nil, nil
}
