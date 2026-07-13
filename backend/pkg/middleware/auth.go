package middleware

import (
	"context"
	"log/slog"
	"strings"
	"time"
	"tokenfactory/pkg/errcode"
	"tokenfactory/pkg/response"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/redis/go-redis/v9"
)

type Claims struct {
	UserID   int64    `json:"user_id"`
	Phone    string   `json:"phone"`
	Roles    []string `json:"roles"`
	jwt.RegisteredClaims
}

func AuthRequired(secret string, rdb *redis.Client) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := extractToken(c)
		if token == "" {
			response.ErrorWithStatus(c, 401, errcode.Unauthorized, errcode.Message(errcode.Unauthorized))
			c.Abort()
			return
		}

		// check blacklist
		ctx := context.Background()
		exists, _ := rdb.Exists(ctx, "session:"+token).Result()
		if exists > 0 {
			response.ErrorWithStatus(c, 401, errcode.Unauthorized, "token已失效")
			c.Abort()
			return
		}

		claims := &Claims{}
		parsed, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (interface{}, error) {
			return []byte(secret), nil
		})
		if err != nil || !parsed.Valid {
			response.ErrorWithStatus(c, 401, errcode.TokenExpired, errcode.Message(errcode.TokenExpired))
			c.Abort()
			return
		}

		c.Set("user_id", claims.UserID)
		c.Set("phone", claims.Phone)
		c.Set("roles", claims.Roles)
		c.Next()
	}
}

func RBAC(requiredRoles ...string) gin.HandlerFunc {
	return func(c *gin.Context) {
		roles, _ := c.Get("roles")
		roleList, _ := roles.([]string)
		for _, required := range requiredRoles {
			for _, r := range roleList {
				if r == required {
					c.Next()
					return
				}
			}
		}
		response.Error(c, errcode.Forbidden, errcode.Message(errcode.Forbidden))
		c.Abort()
	}
}

func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader("X-Request-ID")
		if id == "" {
			id = generateID()
		}
		c.Set("request_id", id)
		c.Header("X-Request-ID", id)
		c.Next()
	}
}

func Logger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		slog.Info("request",
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"status", c.Writer.Status(),
			"duration_ms", time.Since(start).Milliseconds(),
			"request_id", c.GetString("request_id"),
		)
	}
}

func extractToken(c *gin.Context) string {
	auth := c.GetHeader("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return ""
}

func generateID() string {
	return "req_" + time.Now().Format("20060102150405") + randomStr(6)
}

func randomStr(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = letters[time.Now().UnixNano()%int64(len(letters))]
	}
	return string(b)
}
