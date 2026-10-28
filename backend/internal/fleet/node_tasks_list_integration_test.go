package fleet

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"tokenfactory/internal/scheduler"
	"tokenfactory/pkg/errcode"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// CR-S1 Minor: GET /admin/node-tasks 缺省 state = 全部状态(服务层与 handler 一致, 与 docs/api 一致)。
func TestNodeTasksDefaultStateIsAll(t *testing.T) {
	db := setupFleetDB(t)
	seedManaged(t, db)
	s := newTestFleet(t, db)
	v, err := s.RegisterManagedNode(10, scheduler.ManagedNodeReq{ProductID: 1, NodeName: "gpu-list-1", TotalCards: 2,
		LeasePortStart: 47700, LeasePortEnd: 47709})
	require.NoError(t, err)
	id := v.(*Node).ID
	for _, st := range []string{TaskPending, TaskRunning, TaskSucceeded, TaskFailed, TaskDead} {
		db.MustExec(`INSERT INTO node_tasks (node_id, kind, state, priority, payload) VALUES (?, 'probe', ?, 5, '{"mode":"full"}')`, id, st)
	}

	list, total, err := s.Tasks("", 0, 1, 20)
	require.NoError(t, err)
	assert.EqualValues(t, 5, total)
	assert.Len(t, list, 5)
	list, total, err = s.Tasks(TaskDead, id, 1, 20)
	require.NoError(t, err)
	assert.EqualValues(t, 1, total)
	assert.Equal(t, TaskDead, list[0].State)
	_, _, err = s.Tasks("bogus", 0, 1, 20)
	assert.Equal(t, errcode.ParamInvalid, codeOf(err))

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("user_id", int64(1)) })
	NewHandler(s).RegisterAdminRoutes(r.Group("/api/v1"))
	get := func(path string) (int, int64) {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		require.Equal(t, http.StatusOK, w.Code)
		var env struct {
			Code int `json:"code"`
			Data struct {
				Total int64 `json:"total"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env), w.Body.String())
		return env.Code, env.Data.Total
	}
	code, n := get("/api/v1/admin/node-tasks")
	assert.Equal(t, 0, code)
	assert.EqualValues(t, 5, n, "不带 state 返回全部状态")
	code, n = get(fmt.Sprintf("/api/v1/admin/node-tasks?state=failed&node_id=%d", id))
	assert.Equal(t, 0, code)
	assert.EqualValues(t, 1, n)
	code, _ = get("/api/v1/admin/node-tasks?state=bogus")
	assert.Equal(t, errcode.ParamInvalid, code)
}
