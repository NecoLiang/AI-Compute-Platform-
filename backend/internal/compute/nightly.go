package compute

// 包夜(docs/26 §26.6, T-094): 夜晚标识、可订范围、下单日期校验与夜间日历。
//
// "某晚"以窗口开始的日期标识(night_date, Asia/Shanghai)。可订规则:
//   - 当晚: 窗口开始后 2 小时内仍可订, 按整晚计价(Q-36);
//   - night_start_date 最晚为今天起第 30 天(§26.7);
//   - 单笔最多 31 晚(MaxDurationFor), 所有晚落在可订范围内(最后一晚不晚于今天起第 NightHorizonDays 天)。
// 只适用于夜间窗口 + 平台托管 + 分卡容器商品(Q-44; 整机包夜每晚都须重装, 不开放)。

import (
	"context"
	"fmt"
	"time"

	"tokenfactory/pkg/errcode"
)

const (
	// NightSameNightCutoff 当晚预订截止: 窗口开始后 2 小时(Q-36)。
	NightSameNightCutoff = 2 * time.Hour
	// NightStartAheadDays night_start_date 最晚为今天起第 30 天(§26.7)。
	NightStartAheadDays = 30
	// NightHorizonDays 可订范围的最后一晚(今天起第 60 天): 第 30 天起订最多 31 晚。
	NightHorizonDays = 60
	// NightCalendarMaxDays 夜间日历单次最多查询 31 晚: 自今天起查满即覆盖今天..今天起第 30 天,
	// 最后一个可选起始夜晚(NightStartAheadDays)也能在日历上选到(§26.9 原写 30 晚, 差一晚)。
	NightCalendarMaxDays = NightStartAheadDays + 1
	// NightCalendarDefaultDays 夜间日历缺省查询晚数。
	NightCalendarDefaultDays = 14
)

// NightAvailability 夜间日历的一晚: 当晚单节点最多可订的同机卡数(已按可选卡数/整机规则取整), 不可订为 0。
type NightAvailability struct {
	NightDate string `json:"night_date"`
	MaxCards  int    `json:"max_cards"`
}

var errNightlyProductInvalid = errcode.New(errcode.ParamInvalid, "包夜仅适用于平台托管的夜间窗口分卡容器商品")

// Length 窗口时长。
func (w NightWindow) Length() time.Duration { return time.Duration(w.lengthMin()) * time.Minute }

// NightDate 把任意时刻/DATE 列值归一为 Asia/Shanghai 当天零点(按年月日取, 不做时区换算:
// DATE 列读出时的 Location 取决于 DSN 的 loc, 年月日才是业务含义)。
func NightDate(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, shanghaiLoc())
}

// NightStartAt 某晚窗口开始时刻。
func NightStartAt(nightDate time.Time, w NightWindow) time.Time {
	return NightDate(nightDate).Add(time.Duration(w.StartMin) * time.Minute)
}

// NightEndAt 某晚窗口结束时刻(跨零点为次日)。
func NightEndAt(nightDate time.Time, w NightWindow) time.Time {
	return NightStartAt(nightDate, w).Add(w.Length())
}

// FirstBookableNight now 时刻可订的第一晚: 当前所在的晚窗口开始未超过 2 小时则为当晚, 否则为下一晚。
func FirstBookableNight(now time.Time, w NightWindow) time.Time {
	d := NightDateOf(now, w)
	if now.Before(NightStartAt(d, w).Add(NightSameNightCutoff)) {
		return d
	}
	return d.AddDate(0, 0, 1)
}

// LastStartedNight 窗口开始时刻不晚于 now 的最后一晚(night_date 晚于它的晚都尚未开始)。
func LastStartedNight(now time.Time, w NightWindow) time.Time {
	return NightDate(now.In(shanghaiLoc()).Add(-time.Duration(w.StartMin) * time.Minute))
}

func todayIn(now time.Time) time.Time { return NightDate(now.In(shanghaiLoc())) }

// NightOrderWindow 包夜下单/报价的商品前置条件(Q-44): 夜间窗口 + 平台托管 + 分卡容器。返回商品窗口。
func NightOrderWindow(p *Product) (NightWindow, error) {
	w := NightWindowOf(p)
	if w == nil || p.FulfillmentMode != FulfillmentManaged || p.DeliveryMode != "container" {
		return NightWindow{}, errNightlyProductInvalid
	}
	return *w, nil
}

// ParseNightStartDate 校验包夜起订日期与晚数(40001): YYYY-MM-DD; 不早于可订的第一晚(当晚截止为窗口开始后 2 小时);
// 不晚于今天起第 30 天; 最后一晚不超出可订范围。空串返回 40001(包夜必填)。
func ParseNightStartDate(s string, nights int, w NightWindow, now time.Time) (time.Time, error) {
	if s == "" {
		return time.Time{}, errcode.New(errcode.ParamInvalid, "包夜订单须选择起始夜晚 night_start_date(YYYY-MM-DD)")
	}
	d, err := time.ParseInLocation("2006-01-02", s, shanghaiLoc())
	if err != nil {
		return time.Time{}, errcode.New(errcode.ParamInvalid, "night_start_date 格式应为 YYYY-MM-DD")
	}
	first, today := FirstBookableNight(now, w), todayIn(now)
	if d.Before(first) {
		return time.Time{}, errcode.New(errcode.ParamInvalid, fmt.Sprintf("%s 晚已截止预订(窗口开始 2 小时后不可再订), 最早可订 %s 晚",
			s, first.Format("2006-01-02")))
	}
	if d.After(today.AddDate(0, 0, NightStartAheadDays)) {
		return time.Time{}, errcode.New(errcode.ParamInvalid, fmt.Sprintf("包夜起始夜晚须在今天起 %d 天内", NightStartAheadDays))
	}
	if nights > 0 && d.AddDate(0, 0, nights-1).After(today.AddDate(0, 0, NightHorizonDays)) {
		return time.Time{}, errcode.New(errcode.ParamInvalid, fmt.Sprintf("包夜最后一晚超出可订范围(今天起 %d 天内)", NightHorizonDays))
	}
	return d, nil
}

// nightLeasePreview 包夜租期预览: 起点 = max(当前时刻, 首晚窗口开始)(当晚订单以实际开通时刻起算), 终点固定。
func nightLeasePreview(start time.Time, nights int, w NightWindow, now time.Time) *LeasePreview {
	from := NightStartAt(start, w)
	if now.After(from) {
		from = now.In(shanghaiLoc()).Truncate(time.Second)
	}
	end := NightlyLeaseEnd(start, nights, w)
	return &LeasePreview{StartRule: LeaseStartNightWindow, StartAt: &from, EndAt: &end}
}

// NightCalendar GET /products/:id/night-calendar: 自 from 起 days 晚, 每晚单节点最多可订卡数(不加锁快照, 下单以预订唯一键为准)。
// from 为空取可订的第一晚; days 缺省 14、上限 31; cards>0 时只统计能容纳该卡数的节点(max_cards 不足 cards 记 0)。
// 已截止或超出可订范围的晚 max_cards=0。每用户每分钟与 availability 共用 30 次限流。
func (s *Service) NightCalendar(ctx context.Context, userID, productID int64, cards int, fromStr string, days int) ([]NightAvailability, error) {
	if err := s.allowAvailability(ctx, userID); err != nil {
		return nil, err
	}
	if days == 0 {
		days = NightCalendarDefaultDays
	}
	if days < 0 || days > NightCalendarMaxDays {
		return nil, errcode.New(errcode.ParamInvalid, fmt.Sprintf("days 取值 1-%d", NightCalendarMaxDays))
	}
	if cards < 0 {
		return nil, errcode.New(errcode.ParamInvalid, "cards 须为正整数")
	}
	p, err := s.repo.GetProductByID(productID)
	if err != nil {
		return nil, err
	}
	if p.Status != "active" && p.Status != "sold_out" {
		return nil, fmt.Errorf("product not found")
	}
	w, err := NightOrderWindow(p)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	first, today := FirstBookableNight(now, w), todayIn(now)
	from := first
	if fromStr != "" {
		if from, err = time.ParseInLocation("2006-01-02", fromStr, shanghaiLoc()); err != nil {
			return nil, errcode.New(errcode.ParamInvalid, "from 格式应为 YYYY-MM-DD")
		}
		if from.Before(today.AddDate(0, 0, -1)) || from.After(today.AddDate(0, 0, NightHorizonDays)) {
			return nil, errcode.New(errcode.ParamInvalid, fmt.Sprintf("from 须在今天起 %d 天内", NightHorizonDays))
		}
	}
	if s.managedGate == nil {
		return nil, ErrManagedDisabled
	}
	if s.leaseHooks.NightCalendar == nil {
		return nil, errNightlyHooksNotWired
	}
	counts, err := s.leaseHooks.NightCalendar(p, cards, from, days)
	if err != nil {
		return nil, err
	}
	horizon := today.AddDate(0, 0, NightHorizonDays)
	out := make([]NightAvailability, 0, days)
	for i := 0; i < days; i++ {
		d := from.AddDate(0, 0, i)
		n := 0
		if i < len(counts) && !d.Before(first) && !d.After(horizon) {
			n = counts[i]
		}
		if cards > 0 && n < cards {
			n = 0
		}
		out = append(out, NightAvailability{NightDate: d.Format("2006-01-02"), MaxCards: n})
	}
	return out, nil
}
