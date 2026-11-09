package fleet

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// v1.2 迁移回退口径: 027 起的 down 一律 SIGNAL 拒绝(业务/资金/安全闸门数据须经审计归档), 仅 031(可重新采集的探活遥测)直接删除。
func TestDownMigrationPolicy(t *testing.T) {
	files, err := filepath.Glob("../../migrations/*.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	numRe := regexp.MustCompile(`^(\d{3})_`)
	ddl := regexp.MustCompile(`(?im)^\s*(DROP|ALTER|DELETE|TRUNCATE|RENAME)\b`)
	checked := 0
	for _, f := range files {
		m := numRe.FindStringSubmatch(filepath.Base(f))
		if m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		if n < 27 || n == 31 {
			continue
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		body := string(raw)
		if !strings.Contains(body, "SIGNAL SQLSTATE '45000'") {
			t.Errorf("%s: 027 之后的 down 必须 SIGNAL 拒绝自动回退", filepath.Base(f))
		}
		if ddl.MatchString(body) {
			t.Errorf("%s: down 不得包含 DROP/ALTER/DELETE 等删除语句", filepath.Base(f))
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("未找到 027 之后的 down 迁移, 路径有误")
	}
}

// docker-compose 的 MySQL 初始化按挂载文件名排序执行: 每个 up 迁移都要挂上, 且挂载序号与文件序号一致、升序。
func TestComposeMountsAllMigrations(t *testing.T) {
	raw, err := os.ReadFile("../../docker-compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	mountRe := regexp.MustCompile(`\./migrations/((\d{3})_[^:]+\.up\.sql):/docker-entrypoint-initdb\.d/(\d{3})\.sql`)
	mounted := map[string]bool{}
	last := -1
	for _, m := range mountRe.FindAllStringSubmatch(string(raw), -1) {
		if m[2] != m[3] {
			t.Errorf("%s 挂载为 %s.sql, 序号不一致", m[1], m[3])
		}
		n, _ := strconv.Atoi(m[2])
		if n <= last {
			t.Errorf("%s 挂载顺序非升序", m[1])
		}
		last = n
		mounted[m[1]] = true
	}
	ups, err := filepath.Glob("../../migrations/*.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range ups {
		base := filepath.Base(f)
		if base < "026" {
			continue // v1.2 之前的迁移(含历史上同号的 008/009 两份)不在本检查范围
		}
		if !mounted[base] {
			t.Errorf("docker-compose.yml 未挂载 %s", base)
		}
	}
}
