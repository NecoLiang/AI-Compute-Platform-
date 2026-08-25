package db

import (
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	_ "github.com/go-sql-driver/mysql"
)

// NewMySQL 建立连接池。
//
// 这里对 DSN 做两项强制修正, 因为写错的代价极高且不易察觉:
//
//  1. parseTime=true —— 否则 TIMESTAMP 列扫描进 time.Time 会直接报错。
//  2. loc 必须与 MySQL 会话时区一致 —— 缺省时 go-sql-driver 以 UTC 序列化
//     time.Time, 而本项目 MySQL 会话时区为 +08:00, 两者相差 8 小时。
//     后果不是"显示时间不对"这么轻: payment_expires_at 写入的"15 分钟后"
//     会变成"8 小时前", 于是订单一创建就被关单任务判定超时取消、
//     访问凭证一签发就被吊销。这类缺陷在功能测试里看不出来, 只有定时任务
//     真正跑起来才会集中爆发, 因此在建连处兜底而不是只依赖配置文件写对。
func NewMySQL(dsn string) (*sqlx.DB, error) {
	fixed, warnings := normalizeDSN(dsn)
	for _, w := range warnings {
		slog.Warn("DSN 已自动修正", "detail", w)
	}

	db, err := sqlx.Connect("mysql", fixed)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(time.Hour)
	db.SetConnMaxIdleTime(10 * time.Minute)

	if err := verifyTimezone(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// normalizeDSN 补齐 parseTime / loc, 返回修正后的 DSN 与修正说明。
func normalizeDSN(dsn string) (string, []string) {
	var warnings []string

	i := strings.LastIndex(dsn, "/")
	if i < 0 {
		return dsn, nil // 形态异常, 交给驱动报错
	}
	head, tail := dsn[:i+1], dsn[i+1:]

	dbName, rawQuery := tail, ""
	if j := strings.Index(tail, "?"); j >= 0 {
		dbName, rawQuery = tail[:j], tail[j+1:]
	}

	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		return dsn, nil
	}

	if q.Get("parseTime") != "true" {
		q.Set("parseTime", "true")
		warnings = append(warnings, "已补上 parseTime=true")
	}
	if q.Get("loc") == "" {
		q.Set("loc", "Asia/Shanghai")
		warnings = append(warnings, "未指定 loc, 已设为 Asia/Shanghai 以匹配数据库 +08:00 会话时区")
	}

	out := head + dbName
	if enc := q.Encode(); enc != "" {
		out += "?" + enc
	}
	return out, warnings
}

// verifyTimezone 实测驱动写入与数据库读取是否处在同一时区。
// 用一次真实的往返比较代替对配置的信任: 偏差超过 1 分钟即判定为时区错配并拒绝启动。
func verifyTimezone(db *sqlx.DB) error {
	var dbNow time.Time
	if err := db.Get(&dbNow, "SELECT NOW()"); err != nil {
		return fmt.Errorf("读取数据库时间失败: %w", err)
	}
	if d := time.Since(dbNow); d > time.Minute || d < -time.Minute {
		return fmt.Errorf(
			"应用与数据库时间相差 %v, 极可能是 DSN 的 loc 与 MySQL 会话时区不一致。"+
				"继续启动会导致订单超时/凭证到期判断整体错位, 故拒绝启动。"+
				"请确认 DSN 含 loc=Asia%%2FShanghai 且数据库 time_zone 为 +08:00", d.Round(time.Second))
	}
	return nil
}
