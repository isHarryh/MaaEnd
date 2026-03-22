package itemtransfer

import (
	"encoding/json"
	"sort"

	maa "github.com/MaaXYZ/maa-framework-go/v4"
	"github.com/rs/zerolog/log"
)

// ItemTransferOCRAction finds a target item on the current visible page using
// NND (for grid coordinate frames only) + OCR binary search, then Ctrl+Clicks.
// Used for items that have no NND class ID (no expected) but exist in
// item_order.json's category_order.
type ItemTransferOCRAction struct{}

var _ maa.CustomActionRunner = &ItemTransferOCRAction{}

type ocrActionParams struct {
	ItemName   string `json:"item_name"`
	Descending bool   `json:"descending"`
	Side       string `json:"side"`
}

func (a *ItemTransferOCRAction) Run(ctx *maa.Context, arg *maa.CustomActionArg) bool {
	var params ocrActionParams
	if err := json.Unmarshal([]byte(arg.CustomActionParam), &params); err != nil {
		log.Error().Err(err).Str("component", componentName).Msg("failed to parse OCR action params")
		return false
	}

	if params.ItemName == "" {
		log.Error().Str("component", componentName).Msg("item_name is empty")
		return false
	}

	data, err := loadItemOrderData()
	if err != nil {
		log.Error().Err(err).Str("component", componentName).Msg("failed to load item order data")
		return false
	}

	category := findCategoryByName(data, params.ItemName)
	if category == "" {
		log.Error().
			Str("component", componentName).
			Str("item_name", params.ItemName).
			Msg("item not found in any category_order")
		return false
	}

	categoryOrder := data.CategoryOrder[category]
	if len(categoryOrder) == 0 {
		log.Error().
			Str("component", componentName).
			Str("category", category).
			Msg("category_order is empty")
		return false
	}

	if params.Descending {
		categoryOrder = reversed(categoryOrder)
	}

	targetIdx := indexOf(categoryOrder, params.ItemName)
	if targetIdx < 0 {
		log.Error().
			Str("component", componentName).
			Str("item_name", params.ItemName).
			Str("category", category).
			Msg("item not found in its own category_order")
		return false
	}

	side := inferSide(params.Side, arg.CurrentTaskName)

	nndNode := repoNNDNode
	if side == "bag" {
		nndNode = bagNNDNode
	}

	log.Info().
		Str("component", componentName).
		Str("item_name", params.ItemName).
		Int("target_idx", targetIdx).
		Str("category", category).
		Str("side", side).
		Bool("descending", params.Descending).
		Msg("starting OCR search on current page")

	tasker := ctx.GetTasker()
	ctrl := tasker.GetController()

	if tasker.Stopping() {
		return false
	}

	ctrl.PostScreencap().Wait()
	img, err := ctrl.CacheImage()
	if err != nil {
		log.Error().Err(err).Str("component", componentName).Msg("failed to cache image")
		return false
	}

	items := detectAllItems(ctx, img, nndNode)
	if len(items) == 0 {
		log.Warn().Str("component", componentName).Msg("no items detected on current page")
		return false
	}

	cols := repoCols
	if side == "bag" {
		cols = bagCols
	}
	items = buildFullGrid(items, cols)

	log.Info().
		Str("component", componentName).
		Int("grid_count", len(items)).
		Int("cols", cols).
		Msg("full grid built from NND detections")

	result := binarySearchOnPage(ctx, tasker, ctrl, items, categoryOrder, targetIdx, params.ItemName)
	if result != nil {
		return ctrlClick(ctrl, result.CenterX, result.CenterY)
	}

	result = linearScanOnPage(ctx, tasker, ctrl, items, params.ItemName)
	if result != nil {
		return ctrlClick(ctrl, result.CenterX, result.CenterY)
	}

	log.Info().Str("component", componentName).Str("item_name", params.ItemName).Msg("OCR search found nothing")
	return false
}

const (
	gridCellSpacing = 69
	repoCols        = 8
	bagCols         = 5
)

// buildFullGrid reconstructs a complete grid from NND detections.
// NND items provide the coordinate framework; the grid is then filled to
// exactly `cols` columns per row. Rows are determined by clustering Y values
// (items within gridCellSpacing/2 of each other belong to the same row).
func buildFullGrid(items []gridItem, cols int) []gridItem {
	if len(items) == 0 {
		return items
	}

	sort.Slice(items, func(i, j int) bool {
		return items[i].CenterY < items[j].CenterY
	})

	type row struct {
		y     int
		count int
	}
	var rows []row
	rows = append(rows, row{y: items[0].CenterY, count: 1})
	for i := 1; i < len(items); i++ {
		last := &rows[len(rows)-1]
		if items[i].CenterY-last.y/last.count <= gridCellSpacing/2 {
			last.y += items[i].CenterY
			last.count++
		} else {
			rows = append(rows, row{y: items[i].CenterY, count: 1})
		}
	}

	rowYs := make([]int, len(rows))
	for i, r := range rows {
		rowYs[i] = r.y / r.count
	}

	minX := items[0].CenterX
	for _, it := range items[1:] {
		if it.CenterX < minX {
			minX = it.CenterX
		}
	}

	colXs := make([]int, cols)
	for c := 0; c < cols; c++ {
		colXs[c] = minX + c*gridCellSpacing
	}

	grid := make([]gridItem, 0, len(rowYs)*cols)
	for _, y := range rowYs {
		for _, x := range colXs {
			grid = append(grid, gridItem{
				CenterX: x,
				CenterY: y,
				ClassID: ^uint64(0),
			})
		}
	}

	log.Info().
		Str("component", componentName).
		Int("rows", len(rowYs)).
		Int("cols", cols).
		Int("min_x", minX).
		Ints("row_ys", rowYs).
		Msg("grid reconstructed from NND detections")

	return grid
}

func findCategoryByName(data *itemOrderData, name string) string {
	for cat, order := range data.CategoryOrder {
		for _, n := range order {
			if n == name {
				return cat
			}
		}
	}
	return ""
}
