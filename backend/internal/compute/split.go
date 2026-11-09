package compute

// 托管分卡租赁的可选卡数(docs/26 §26.5.1, T-092)。
//
// 只适用于 fulfillment_mode=managed 且 delivery_mode=container 的零租商品:
// 供给方从平台允许集合中勾选, 买家下单的卡数(quantity)必须落在所选集合内, min_order 不再参与校验。
// 整机(bare_metal)与人工商品不可分卡, 不得提交该字段。
//
// S3 实现口径: 平台集合暂为代码常量 {1,2,4,6,8}(docs/26 写的配置项 market.card_split_choices 未落地);
// "所选值不得超过该商品下最大节点卡数"以商品 card_count(标称单机卡数)为上限校验 —— 托管商品先建草稿、
// 后接节点, 发布时节点尚不存在。超出实际节点卡数的选项不会被错误接单: 下单与 availability 以卡槽分配结果为准。

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// PlatformCardSplitChoices 平台允许的可选卡数集合(升序)。
var PlatformCardSplitChoices = []int{1, 2, 4, 6, 8}

// isSplitProduct 托管分卡商品: 零租 + 托管 + 容器交付。
func isSplitProduct(productType, fulfillment, delivery string) bool {
	return productType == ProductTypeCardRental && fulfillment == FulfillmentManaged && delivery == "container"
}

// defaultSplitOptions 未勾选时的默认集合: 平台集合中不超过单机卡数的全部值。
func defaultSplitOptions(cardCount int) []int {
	out := []int{}
	for _, c := range PlatformCardSplitChoices {
		if cardCount <= 0 || c <= cardCount {
			out = append(out, c)
		}
	}
	return out
}

// normalizeSplitOptions 去重升序。
func normalizeSplitOptions(in []int) []int {
	seen := map[int]bool{}
	out := make([]int, 0, len(in))
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Ints(out)
	return out
}

// validateSplitOptions 发布/重提校验(40001)。
func validateSplitOptions(req CreateProductReq) error {
	split := isSplitProduct(req.ProductType, orDefault(req.FulfillmentMode, FulfillmentManual), req.DeliveryMode)
	if !split {
		if len(req.CardSplitOptions) > 0 {
			return fmt.Errorf("可选卡数仅适用于平台托管的分卡容器(container)零租商品")
		}
		return nil
	}
	if len(req.CardSplitOptions) == 0 {
		return fmt.Errorf("托管分卡商品至少需要一个可选卡数")
	}
	allowed := map[int]bool{}
	for _, c := range PlatformCardSplitChoices {
		allowed[c] = true
	}
	for _, v := range req.CardSplitOptions {
		if !allowed[v] {
			return fmt.Errorf("可选卡数 %d 不在平台允许集合 %s 内", v, FormatCardSplitOptions(PlatformCardSplitChoices))
		}
		if req.CardCount > 0 && v > req.CardCount {
			return fmt.Errorf("可选卡数 %d 超过商品单机卡数 %d", v, req.CardCount)
		}
	}
	return nil
}

// FormatCardSplitOptions 库内存储格式 "1,2,4,8"(VARCHAR(32))。
func FormatCardSplitOptions(opts []int) string {
	parts := make([]string, len(opts))
	for i, v := range opts {
		parts[i] = strconv.Itoa(v)
	}
	return strings.Join(parts, ",")
}

// ParseCardSplitOptions 解析库内存储格式; 非法片段跳过(只会由本模块写入)。
func ParseCardSplitOptions(raw string) []int {
	out := []int{}
	for _, s := range strings.Split(raw, ",") {
		if v, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && v > 0 {
			out = append(out, v)
		}
	}
	return normalizeSplitOptions(out)
}

// SplitOptions 商品的可选卡数。非分卡商品返回 nil; 分卡商品列为 NULL(存量/未勾选)时取默认集合。
func (p *Product) SplitOptions() []int {
	if p == nil || !isSplitProduct(p.ProductType, p.FulfillmentMode, p.DeliveryMode) {
		return nil
	}
	if p.CardSplitOptionsRaw != nil && strings.TrimSpace(*p.CardSplitOptionsRaw) != "" {
		if opts := ParseCardSplitOptions(*p.CardSplitOptionsRaw); len(opts) > 0 {
			return opts
		}
	}
	return defaultSplitOptions(p.CardCount)
}

func containsInt(list []int, v int) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
