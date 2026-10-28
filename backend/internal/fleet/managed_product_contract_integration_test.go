package fleet

import (
	"testing"

	"tokenfactory/internal/compute"
	"tokenfactory/internal/legal"
	"tokenfactory/pkg/errcode"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// CR-S1 Minor: compute-api.md 记载的托管商品契约(fulfillment_mode / 托管草稿 / 提审与下单错误)以真实服务为准。
// 错误码按 handler 实际使用的 compute.ErrToCode 断言。
func TestManagedProductDocumentedContract(t *testing.T) {
	db := setupFleetDB(t)
	s := newTestFleet(t, db)
	comp, ok := s.products.(*compute.Service)
	require.True(t, ok)
	db.MustExec(`INSERT INTO users (id, phone, password_hash) VALUES (30,'13800000030','x'),(31,'13800000031','x')`)
	db.MustExec(`INSERT INTO user_roles (user_id, role) VALUES (30,'supplier'),(31,'buyer')`)
	db.MustExec(`INSERT INTO enterprises (user_id, name, uscc, status) VALUES (30,'供给方','TESTSUP30','verified'),(31,'买家','TESTBUY31','verified')`)
	db.MustExec(`INSERT INTO supplier_qualifications (user_id, qual_type, cert_name, status) VALUES (30,'idc','IDC 许可','verified')`)
	db.MustExec(`UPDATE gpu_catalog SET probe_baseline_tflops=600 WHERE model_name='H100-80G'`)

	req := func(mut func(r *compute.CreateProductReq)) compute.CreateProductReq {
		r := compute.CreateProductReq{ProductType: compute.ProductTypeCardRental, GpuModel: "H100-80G", CardCount: 2,
			DeliveryMode: "container", FulfillmentMode: compute.FulfillmentManaged, PricingMode: "hourly", UnitPrice: 1500,
			Stock: 99, Region: "cn-north", ComplianceAgreed: true, ComplianceVersion: legal.Version}
		if mut != nil {
			mut(&r)
		}
		return r
	}
	code := func(err error) int {
		require.Error(t, err)
		return compute.ErrToCode(err)
	}
	product := func(id int64) *compute.Product {
		p, err := comp.ManagedProduct(id)
		require.NoError(t, err)
		require.NotNil(t, p)
		return p
	}

	// 发布: 托管商品为草稿, stock 忽略人工填写值; 非零租 / 非 container|bare_metal / 非法模式 40001。
	id, err := comp.CreateProduct(30, req(nil))
	require.NoError(t, err)
	p := product(id)
	assert.Equal(t, "draft", p.Status)
	assert.Equal(t, compute.FulfillmentManaged, p.FulfillmentMode)
	assert.Equal(t, 0, p.Stock)
	assert.Equal(t, errcode.ParamInvalid, code(firstErr(comp.CreateProduct(30, req(func(r *compute.CreateProductReq) { r.DeliveryMode = "vm" })))))
	assert.Equal(t, errcode.ParamInvalid, code(firstErr(comp.CreateProduct(30, req(func(r *compute.CreateProductReq) { r.FulfillmentMode = "auto" })))))
	manualID, err := comp.CreateProduct(30, req(func(r *compute.CreateProductReq) { r.FulfillmentMode = "" }))
	require.NoError(t, err)
	assert.Equal(t, "pending", product(manualID).Status, "人工商品(缺省)创建即进入审核")
	assert.Equal(t, compute.FulfillmentManual, product(manualID).FulfillmentMode)

	// fleet 未启用(未注入闸门): 提审 40001。
	err = comp.ResubmitProduct(30, id, req(nil))
	assert.Equal(t, errcode.ParamInvalid, code(err))
	assert.ErrorIs(t, err, compute.ErrManagedDisabled)

	comp.SetManagedGate(s)
	// 没有托管节点: 40001; 履约模式不可改: 40001。
	err = comp.ResubmitProduct(30, id, req(nil))
	assert.Equal(t, errcode.ParamInvalid, code(err))
	assert.Contains(t, err.Error(), "须至少接入一个 ssh 托管节点")
	err = comp.ResubmitProduct(30, id, req(func(r *compute.CreateProductReq) { r.FulfillmentMode = compute.FulfillmentManual }))
	assert.Equal(t, errcode.ParamInvalid, code(err))
	assert.Contains(t, err.Error(), "履约模式创建后不可修改")

	// 节点未 verified: 40001 并逐项列出原因。
	res := db.MustExec(`INSERT INTO supplier_nodes (supplier_id, product_id, node_name, status, total_cards, available_cards,
		mgmt_mode, ssh_host, ssh_port, ssh_user, verify_status, schedulable, disk_quota_supported, cpu_cores, mem_gb,
		lease_port_start, lease_port_end) VALUES (30, ?, 'c1', 'online', 2, 2, 'ssh', '203.0.113.9', 22, 'root', 'unverified', 1, 1, 64, 512, 30000, 30099)`, id)
	nodeID, _ := res.LastInsertId()
	err = comp.ResubmitProduct(30, id, req(nil))
	assert.Equal(t, errcode.ParamInvalid, code(err))
	assert.Contains(t, err.Error(), "节点 c1: 核验状态 unverified")

	// verified + 型号一致的通过报告: 提审 → pending; 审核通过 → active 且库存 = 空闲卡槽。
	db.MustExec(`UPDATE supplier_nodes SET verify_status='verified' WHERE id=?`, nodeID)
	db.MustExec(`INSERT INTO node_probe_reports (node_id, kind, result, checks, inventory) VALUES (?, 'verify', 'pass', '[]',
		'{"gpus":[{"index":0,"name":"NVIDIA H100 80GB HBM3"},{"index":1,"name":"NVIDIA H100 80GB HBM3"}]}')`, nodeID)
	for g := 0; g < 2; g++ {
		db.MustExec(`INSERT INTO gpu_slots (node_id, gpu_index, gpu_uuid, pci_bus_id, vram_mb, topo_group) VALUES (?,?,?,?,81920,'g0')`,
			nodeID, g, "GPU-contract-"+string(rune('a'+g)), "0000:0"+string(rune('1'+g))+":00.0")
	}
	require.NoError(t, comp.ResubmitProduct(30, id, req(nil)))
	assert.Equal(t, "pending", product(id).Status)
	require.NoError(t, comp.ApproveProduct(id))
	p = product(id)
	assert.Equal(t, "active", p.Status)
	assert.Equal(t, 2, p.Stock, "托管库存由卡槽派生")

	// 下单: 租约钩子未接入 50000; fleet 未启用 40001; 买断计费 40001。
	order := func() error {
		_, err := comp.PlaceOrder(31, compute.PlaceOrderReq{ProductID: id, Quantity: 1, Duration: 1,
			ComplianceAgreed: true, ComplianceVersion: legal.Version})
		return err
	}
	assert.Equal(t, errcode.InternalError, code(order()))
	comp.SetManagedGate(nil)
	assert.Equal(t, errcode.ParamInvalid, code(order()))
	comp.SetManagedGate(s)
	comp.SetLeaseHooks(s.LeaseHooks())
	db.MustExec(`UPDATE products SET pricing_mode='perpetual' WHERE id=?`, id)
	err = order()
	assert.Equal(t, errcode.ParamInvalid, code(err))
	db.MustExec(`UPDATE products SET pricing_mode='hourly' WHERE id=?`, id)
	// 需要的卡数超过任一节点余量: 40900。
	_, err = comp.PlaceOrder(31, compute.PlaceOrderReq{ProductID: id, Quantity: 2, Duration: 1,
		ComplianceAgreed: true, ComplianceVersion: legal.Version})
	require.NoError(t, err)
	db.MustExec(`UPDATE products SET stock=1, status='active' WHERE id=?`, id) // 绕过派生库存的前置拦截, 直达卡槽预留
	assert.Equal(t, errcode.Conflict, code(order()))
}

func firstErr(_ int64, err error) error { return err }
