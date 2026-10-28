package compute

import (
	"cmp"
	"slices"

	"github.com/jmoiron/sqlx"
)

// ===== product_prices 读写 (028, T-090) =====

// attachPrices 批量为商品装载全部价格行(含已停用, PriceFor 需据此区分"停用"与"无价格行回退")。
// 按 product_id 一次 IN 查询, 避免列表 N+1。市场展示再用 EnabledPrices 过滤。
func attachPrices(q sqlx.Queryer, ps []*Product) error {
	if len(ps) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(ps))
	byID := make(map[int64][]*Product, len(ps))
	for _, p := range ps {
		p.Prices = []ProductPrice{}
		if _, seen := byID[p.ID]; !seen {
			ids = append(ids, p.ID)
		}
		byID[p.ID] = append(byID[p.ID], p)
	}
	query, args, err := sqlx.In(`SELECT product_id, pricing_mode, unit_price, min_duration, enabled
		FROM product_prices WHERE product_id IN (?)`, ids)
	if err != nil {
		return err
	}
	var rows []ProductPrice
	if err := sqlx.Select(q, &rows, query, args...); err != nil {
		return err
	}
	sortPrices(rows)
	for _, row := range rows {
		for _, p := range byID[row.ProductID] {
			p.Prices = append(p.Prices, row)
		}
	}
	return nil
}

func attachPricesToSlice(q sqlx.Queryer, list []Product) error {
	ps := make([]*Product, len(list))
	for i := range list {
		ps[i] = &list[i]
	}
	return attachPrices(q, ps)
}

// sortPrices 按商品、周期从短到长稳定排序, 保证接口输出顺序可预期。
func sortPrices(rows []ProductPrice) {
	slices.SortStableFunc(rows, func(a, b ProductPrice) int {
		if c := cmp.Compare(a.ProductID, b.ProductID); c != 0 {
			return c
		}
		return cmp.Compare(periodRank(a.PricingMode), periodRank(b.PricingMode))
	})
}

// savePricesTx 在商品写入的同一事务内保存价格: 提交的方式逐行 upsert, 未提交的方式置为停用
// (不删除: 存量订单续租需要据此判定"该方式已停用"并返回 40900)。prices 为空即全部停用(非零租商品)。
func savePricesTx(tx *sqlx.Tx, productID int64, prices []ProductPrice) error {
	modes := make([]string, 0, len(prices))
	for _, pr := range prices {
		if _, err := tx.Exec(`INSERT INTO product_prices (product_id, pricing_mode, unit_price, min_duration, enabled)
			VALUES (?,?,?,?,?) ON DUPLICATE KEY UPDATE unit_price=VALUES(unit_price),
			min_duration=VALUES(min_duration), enabled=VALUES(enabled)`,
			productID, pr.PricingMode, pr.UnitPrice, pr.MinDuration, pr.Enabled); err != nil {
			return err
		}
		modes = append(modes, pr.PricingMode)
	}
	if len(modes) == 0 {
		_, err := tx.Exec("UPDATE product_prices SET enabled=0 WHERE product_id=?", productID)
		return err
	}
	query, args, err := sqlx.In("UPDATE product_prices SET enabled=0 WHERE product_id=? AND pricing_mode NOT IN (?)", productID, modes)
	if err != nil {
		return err
	}
	_, err = tx.Exec(query, args...)
	return err
}
