package fleet

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// ===== 访问网关存活(gateway_down, docs/25 §25.7.6) =====
//
// 访问网关每 accessgw.PollInterval(5s)经 HMAC 签名调用主服务内部接口(撤销轮询 + 会话上报), 验签通过即在 Redis
// 记录最后一次请求时间。停机扫描据此认定网关不可用: 最后一次请求早于 gatewayStaleAfter → 所有在租租约记一段
// gateway_down(平台责任), 开始时间 = 最后一次请求时间; 网关恢复上报后按原有规则结束(容器租约等对账确认在跑)。
//
// 只有本进程挂载了网关内部接口(NewGatewayAPI)才判定; 从未收到过网关请求(键不存在)、Redis 读失败视为"未知", 不认定。
// 主服务自身启动后 gatewayStaleAfter 内不判定: 主服务停机期间网关无法上报, 那段时间不归为网关故障。

const (
	gatewayHeartbeatKey = "accessgw:heartbeat"
	// gatewayStaleAfter 网关 5s 轮询一次; 连续约 36 轮无请求才认定, 吸收网络抖动与网关滚动重启。
	gatewayStaleAfter = 3 * time.Minute
)

// touchGatewayHeartbeat 验签通过后记录网关最后请求时间(毫秒)。失败只记日志, 不影响本次请求。
func (g *GatewayAPI) touchGatewayHeartbeat(ctx context.Context) {
	if err := g.rdb.Set(ctx, gatewayHeartbeatKey, g.now().UnixMilli(), 0).Err(); err != nil {
		slog.Warn("记录访问网关心跳失败(Redis)", "error", err)
	}
}

// gatewayDownSince 网关是否已失联(返回最后一次请求时间)。未挂载网关接口 / 启动宽限期内 / 心跳未知时返回 false。
func (s *Service) gatewayDownSince(ctx context.Context, now time.Time) (time.Time, bool) {
	if !s.gwWatch || s.rdb == nil || now.Sub(s.startedAt) < gatewayStaleAfter {
		return time.Time{}, false
	}
	v, err := s.rdb.Get(ctx, gatewayHeartbeatKey).Result()
	if errors.Is(err, redis.Nil) {
		return time.Time{}, false
	}
	if err != nil {
		slog.Warn("读取访问网关心跳失败(Redis), 本轮不判定网关存活", "error", err)
		return time.Time{}, false
	}
	ms, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		slog.Warn("访问网关心跳值非法, 本轮不判定网关存活", "value", v)
		return time.Time{}, false
	}
	last := time.UnixMilli(ms)
	if now.Sub(last) < gatewayStaleAfter {
		return time.Time{}, false
	}
	return last, true
}
