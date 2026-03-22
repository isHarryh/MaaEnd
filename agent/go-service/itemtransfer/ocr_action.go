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

	sortByGridPosition(items)
	items = fillGridGaps(items)

	log.Info().
		Str("component", componentName).
		Int("grid_count_after_fill", len(items)).
		Msg("grid cells after gap filling")

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

const gridCellSpacing = 69

// fillGridGaps uses the known grid cell spacing (~69px) to insert synthetic
// grid cells wherever NND missed consecutive items (no NND class).
// For each row, gap / gridCellSpacing is rounded to the nearest integer to
// determine the number of columns spanned; any span > 1 means missing cells.
func fillGridGaps(items []gridItem) []gridItem {
	if len(items) < 2 {
		return items
	}

	const rowGap = 20

	type rowRange struct{ start, end int }
	var rows []rowRange
	rStart := 0
	for i := 1; i < len(items); i++ {
		if items[i].CenterY-items[i-1].CenterY > rowGap {
			rows = append(rows, rowRange{rStart, i})
			rStart = i
		}
	}
	rows = append(rows, rowRange{rStart, len(items)})

	var result []gridItem
	for _, r := range rows {
		rowY := items[r.start].CenterY
		for i := r.start; i < r.end; i++ {
			if i > r.start {
				gap := items[i].CenterX - items[i-1].CenterX
				span := (gap + gridCellSpacing/2) / gridCellSpacing
				if span > 1 {
					step := gap / span
					for m := 1; m < span; m++ {
						syntheticX := items[i-1].CenterX + m*step
						result = append(result, gridItem{
							CenterX: syntheticX,
							CenterY: rowY,
							ClassID: ^uint64(0),
							Score:   0,
						})
					}
					log.Info().
						Str("component", componentName).
						Int("gap", gap).
						Int("span", span).
						Int("inserted", span-1).
						Int("after_x", items[i-1].CenterX).
						Int("before_x", items[i].CenterX).
						Int("row_y", rowY).
						Msg("filled grid gap with synthetic cells")
				}
			}
			result = append(result, items[i])
		}
	}

	sort.Slice(result, func(i, j int) bool {
		if abs(result[i].CenterY-result[j].CenterY) <= rowGap {
			return result[i].CenterX < result[j].CenterX
		}
		return result[i].CenterY < result[j].CenterY
	})

	return result
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
