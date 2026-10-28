package fleet

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"

	"tokenfactory/pkg/accessgw"
	"tokenfactory/pkg/errcode"
	"tokenfactory/pkg/middleware"
	"tokenfactory/pkg/response"

	"github.com/gin-gonic/gin"
)

const gwMaxBody = 1 << 20

// Router 访问网关内部接口(docs/25 §25.6.2)。由 cmd/server 挂在独立的 http.Server(accessgw.internal_listen)上,
// 与公网路由完全分开; 所有请求都要过 HMAC 验签 + nonce 去重, 失败 40100。
func (g *GatewayAPI) Router() http.Handler {
	r := gin.New()
	r.Use(gin.Recovery(), middleware.RequestID(), g.verify)
	grp := r.Group(accessgw.PathPrefix)
	grp.POST("/auth", g.handleAuth)
	grp.GET("/revocations", g.handleRevocations)
	grp.POST("/sessions", g.handleSessions)
	grp.POST("/probe-result", g.handleProbeResult)
	r.NoRoute(func(c *gin.Context) { response.Error(c, errcode.NotFound, "接口不存在") })
	return r
}

// verify 签名覆盖 method、RequestURI(含查询串)、时间戳、nonce 与 body 摘要; nonce 用 Redis SET NX EX 300 去重。
func (g *GatewayAPI) verify(c *gin.Context) {
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, gwMaxBody+1))
	if err != nil || len(body) > gwMaxBody {
		g.reject(c, "请求体过大或读取失败")
		return
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	h := c.Request.Header
	nonce := h.Get(accessgw.HeaderNonce)
	if err := accessgw.Verify(g.secret, c.Request.Method, c.Request.URL.RequestURI(), h.Get(accessgw.HeaderTimestamp),
		nonce, h.Get(accessgw.HeaderSignature), body, g.now()); err != nil {
		g.reject(c, err.Error())
		return
	}
	ok, err := g.rdb.SetNX(c.Request.Context(), nonceKey(nonce), 1, accessgw.NonceTTL).Result()
	if err != nil {
		slog.Error("访问网关 nonce 去重失败(Redis)", "error", err)
		c.Abort()
		response.Error(c, errcode.InternalError, "服务内部错误, 请稍后重试")
		return
	}
	if !ok {
		g.reject(c, "nonce 重放")
		return
	}
	c.Next()
}

func (g *GatewayAPI) reject(c *gin.Context, why string) {
	slog.Warn("访问网关内部接口验签失败", "path", c.Request.URL.Path, "remote", c.ClientIP(), "reason", why)
	c.Abort()
	response.Error(c, errcode.Unauthorized, "签名校验失败")
}

func (g *GatewayAPI) handleAuth(c *gin.Context) {
	var req accessgw.AuthRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, errcode.ParamInvalid, "请求体非法")
		return
	}
	out, err := g.Authenticate(c.Request.Context(), req)
	if err != nil {
		fail(c, err)
		return
	}
	response.Success(c, out)
}

func (g *GatewayAPI) handleRevocations(c *gin.Context) {
	out, err := g.Revocations(c.Query("cursor"))
	if err != nil {
		fail(c, err)
		return
	}
	response.Success(c, out)
}

func (g *GatewayAPI) handleSessions(c *gin.Context) {
	var req accessgw.SessionsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, errcode.ParamInvalid, "请求体非法")
		return
	}
	out, err := g.RecordSessions(req.Events)
	if err != nil {
		fail(c, err)
		return
	}
	response.Success(c, out)
}

func (g *GatewayAPI) handleProbeResult(c *gin.Context) {
	var req accessgw.ProbeResult
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, errcode.ParamInvalid, "请求体非法")
		return
	}
	if err := g.ProbeResult(c.Request.Context(), req); err != nil {
		fail(c, err)
		return
	}
	response.Success(c, gin.H{})
}
