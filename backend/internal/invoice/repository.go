package invoice

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
)

// ===== Models =====

// InvoiceTitle 买家开票信息(抬头), 每买家一份。
// address/phone 为专票预留字段, 本迭代 UI 按设计稿只暴露四个必填项。
type InvoiceTitle struct {
	ID          int64     `db:"id" json:"id"`
	BuyerID     int64     `db:"buyer_id" json:"buyer_id"`
	TitleType   string    `db:"title_type" json:"title_type"`
	CompanyName string    `db:"company_name" json:"company_name"`
	TaxNo       string    `db:"tax_no" json:"tax_no"`
	BankName    string    `db:"bank_name" json:"bank_name"`
	BankAccount string    `db:"bank_account" json:"bank_account"`
	Address     *string   `db:"address" json:"address"`
	Phone       *string   `db:"phone" json:"phone"`
	CreatedAt   time.Time `db:"created_at" json:"created_at"`
	UpdatedAt   time.Time `db:"updated_at" json:"updated_at"`
}

// Invoice 发票记录。抬头为申请时快照, 之后修改 invoice_titles 不影响本行。
// PDFBlob 绝不随 JSON 出站(列表/详情查询不选该列), 仅 download 专用查询读取。
type Invoice struct {
	ID           int64      `db:"id" json:"id"`
	InvoiceNo    string     `db:"invoice_no" json:"invoice_no"`
	BuyerID      int64      `db:"buyer_id" json:"buyer_id"`
	CompanyName  string     `db:"company_name" json:"company_name"`
	TaxNo        string     `db:"tax_no" json:"tax_no"`
	BankName     string     `db:"bank_name" json:"bank_name"`
	BankAccount  string     `db:"bank_account" json:"bank_account"`
	AmountFen    int64      `db:"amount_fen" json:"amount_fen"`
	InvoiceType  string     `db:"invoice_type" json:"invoice_type"`
	Status       string     `db:"status" json:"status"`
	TaxInvoiceNo *string    `db:"tax_invoice_no" json:"tax_invoice_no"`
	PDFBlob      []byte     `db:"pdf_blob" json:"-"`
	PDFFilename  *string    `db:"pdf_filename" json:"pdf_filename,omitempty"`
	RejectReason *string    `db:"reject_reason" json:"reject_reason"`
	AppliedAt    time.Time  `db:"applied_at" json:"applied_at"`
	IssuedAt     *time.Time `db:"issued_at" json:"issued_at"`
	CreatedAt    time.Time  `db:"created_at" json:"-"`
	UpdatedAt    time.Time  `db:"updated_at" json:"-"`
}

// BillableOrder 可开票订单(已付款且未被 pending/issued 发票占用)。
type BillableOrder struct {
	OrderID     int64     `db:"id" json:"-"`
	OrderNo     string    `db:"order_no" json:"order_no"`
	Status      string    `db:"status" json:"status"`
	Quantity    int       `db:"quantity" json:"quantity"`
	TotalAmount int64     `db:"total_amount" json:"total_amount"`
	GPUModel    string    `db:"gpu_model" json:"gpu_model"`
	ProductType string    `db:"product_type" json:"product_type"`
	PricingMode string    `db:"pricing_mode" json:"pricing_mode"`
	CreatedAt   time.Time `db:"created_at" json:"created_at"`
}

const titleColumns = `id, buyer_id, title_type, company_name, tax_no, bank_name, bank_account,
	address, phone, created_at, updated_at`

// invoiceColumns 不含 pdf_blob: 大字段只在 download 时单独读取。
const invoiceColumns = `id, invoice_no, buyer_id, company_name, tax_no, bank_name, bank_account,
	amount_fen, invoice_type, status, tax_invoice_no, pdf_filename, reject_reason,
	applied_at, issued_at, created_at, updated_at`

const billableOrderColumns = `o.id, o.order_no, o.status, o.quantity, o.total_amount,
	COALESCE(p.gpu_model,'') AS gpu_model, COALESCE(p.product_type,'') AS product_type,
	COALESCE(p.pricing_mode,'') AS pricing_mode, o.created_at`

// ===== Repository =====

type Repository struct {
	db *sqlx.DB
}

func NewRepository(db *sqlx.DB) *Repository {
	return &Repository{db: db}
}

// ---- Title ----

func (r *Repository) GetTitle(buyerID int64) (*InvoiceTitle, error) {
	var t InvoiceTitle
	err := r.db.Get(&t, "SELECT "+titleColumns+" FROM invoice_titles WHERE buyer_id = ?", buyerID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &t, err
}

func (r *Repository) UpsertTitle(t *InvoiceTitle) error {
	_, err := r.db.Exec(
		`INSERT INTO invoice_titles (buyer_id, title_type, company_name, tax_no, bank_name, bank_account, address, phone)
		VALUES (?,?,?,?,?,?,?,?)
		ON DUPLICATE KEY UPDATE
		title_type=VALUES(title_type), company_name=VALUES(company_name), tax_no=VALUES(tax_no),
		bank_name=VALUES(bank_name), bank_account=VALUES(bank_account),
		address=VALUES(address), phone=VALUES(phone)`,
		t.BuyerID, t.TitleType, t.CompanyName, t.TaxNo, t.BankName, t.BankAccount, t.Address, t.Phone,
	)
	return err
}

// ---- Billable orders ----

// billableWhere 可开票口径: 已付款状态 + 未被任何 pending/issued 发票占用。
// rejected 发票不占用订单, 其关联订单可重新申请。
const billableWhere = `o.buyer_id = ? AND o.status IN ('paid','provisioning','active','completed')
	AND NOT EXISTS (
		SELECT 1 FROM invoice_orders io JOIN invoices i ON i.id = io.invoice_id
		WHERE io.order_id = o.id AND i.status IN ('pending','issued')
	)`

func (r *Repository) ListBillableOrders(buyerID int64) ([]BillableOrder, error) {
	list := make([]BillableOrder, 0)
	err := r.db.Select(&list, fmt.Sprintf(
		`SELECT %s FROM orders o LEFT JOIN products p ON p.id = o.product_id
		WHERE %s ORDER BY o.created_at DESC`, billableOrderColumns, billableWhere), buyerID)
	return list, err
}

// LockBillableOrdersByNos 事务内按订单号锁定并读取订单, 供申请开票校验。
// 调用方必须已开启事务; 返回的订单数可能与 orderNos 不等(需调用方比对)。
func (r *Repository) LockBillableOrdersByNos(tx *sqlx.Tx, buyerID int64, orderNos []string) ([]BillableOrder, error) {
	query := fmt.Sprintf(
		`SELECT %s FROM orders o LEFT JOIN products p ON p.id = o.product_id
		WHERE %s AND o.order_no IN (?) FOR UPDATE`, billableOrderColumns, billableWhere)
	// sqlx.In 会把 slice 参数展开成对应数量的占位符;
	// orderNos 必须作为单个 slice 参数传入, 手动展开会导致占位符与参数数量不匹配。
	query, args, err := sqlx.In(query, buyerID, orderNos)
	if err != nil {
		return nil, err
	}
	list := make([]BillableOrder, 0)
	if err := tx.Select(&list, tx.Rebind(query), args...); err != nil {
		return nil, err
	}
	return list, nil
}

// ---- Invoices ----

// NextInvoiceNo 在事务内生成 INV-YYYY-NNNN 格式的下一张发票编号。
// 用唯一索引兜底并发: 即使两个事务读到同一最大值, 后写者会因 UNIQUE 冲突失败。
func (r *Repository) NextInvoiceNo(tx *sqlx.Tx, year int) (string, error) {
	prefix := fmt.Sprintf("INV-%d-", year)
	var maxNo sql.NullString
	err := tx.Get(&maxNo,
		"SELECT MAX(invoice_no) FROM invoices WHERE invoice_no LIKE ? FOR UPDATE", prefix+"%")
	if err != nil {
		return "", err
	}
	return NextInvoiceNoFromMax(prefix, maxNo.String), nil
}

func (r *Repository) CreateInvoiceTx(tx *sqlx.Tx, inv *Invoice) (int64, error) {
	res, err := tx.Exec(
		`INSERT INTO invoices (invoice_no, buyer_id, company_name, tax_no, bank_name, bank_account,
			amount_fen, invoice_type, status) VALUES (?,?,?,?,?,?,?,?, 'pending')`,
		inv.InvoiceNo, inv.BuyerID, inv.CompanyName, inv.TaxNo, inv.BankName, inv.BankAccount,
		inv.AmountFen, inv.InvoiceType,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ReleaseRejectedLinksTx 清理这些订单在 rejected 发票下的关联行。
// uq_order 是物理唯一约束, 而「驳回释放订单」是查询口径: 若不先清理,
// 被驳回订单再次申请开票时会撞 UNIQUE (Error 1062)。
func (r *Repository) ReleaseRejectedLinksTx(tx *sqlx.Tx, orderIDs []int64) error {
	if len(orderIDs) == 0 {
		return nil
	}
	query, args, err := sqlx.In(
		`DELETE io FROM invoice_orders io JOIN invoices i ON i.id = io.invoice_id
		WHERE io.order_id IN (?) AND i.status = 'rejected'`, orderIDs)
	if err != nil {
		return err
	}
	_, err = tx.Exec(tx.Rebind(query), args...)
	return err
}

func (r *Repository) LinkOrdersTx(tx *sqlx.Tx, invoiceID int64, orderIDs []int64) error {
	for _, id := range orderIDs {
		if _, err := tx.Exec("INSERT INTO invoice_orders (invoice_id, order_id) VALUES (?,?)", invoiceID, id); err != nil {
			return err
		}
	}
	return nil
}

func (r *Repository) ListInvoices(buyerID int64, page, pageSize int) ([]Invoice, int64, error) {
	page, pageSize = normalizePage(page, pageSize)
	var total int64
	if err := r.db.Get(&total, "SELECT COUNT(*) FROM invoices WHERE buyer_id = ?", buyerID); err != nil {
		return nil, 0, err
	}
	list := make([]Invoice, 0)
	err := r.db.Select(&list,
		"SELECT "+invoiceColumns+" FROM invoices WHERE buyer_id = ? ORDER BY applied_at DESC LIMIT ? OFFSET ?",
		buyerID, pageSize, (page-1)*pageSize)
	return list, total, err
}

func (r *Repository) GetInvoiceByNo(buyerID int64, invoiceNo string) (*Invoice, error) {
	var inv Invoice
	err := r.db.Get(&inv,
		"SELECT "+invoiceColumns+" FROM invoices WHERE buyer_id = ? AND invoice_no = ?", buyerID, invoiceNo)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &inv, err
}

func (r *Repository) GetInvoiceByID(id int64) (*Invoice, error) {
	var inv Invoice
	err := r.db.Get(&inv, "SELECT "+invoiceColumns+" FROM invoices WHERE id = ?", id)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &inv, err
}

func (r *Repository) GetInvoicePDF(buyerID int64, invoiceNo string) ([]byte, *string, error) {
	var row struct {
		PDFBlob     []byte  `db:"pdf_blob"`
		PDFFilename *string `db:"pdf_filename"`
	}
	err := r.db.Get(&row,
		"SELECT pdf_blob, pdf_filename FROM invoices WHERE buyer_id = ? AND invoice_no = ? AND status = 'issued'",
		buyerID, invoiceNo)
	if err == sql.ErrNoRows {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	return row.PDFBlob, row.PDFFilename, nil
}

// ---- Admin ----

func (r *Repository) ListAllInvoices(status string, page, pageSize int) ([]Invoice, int64, error) {
	page, pageSize = normalizePage(page, pageSize)
	where := ""
	args := []interface{}{}
	if status != "" {
		where = "WHERE status = ?"
		args = append(args, status)
	}
	var total int64
	if err := r.db.Get(&total, "SELECT COUNT(*) FROM invoices "+where, args...); err != nil {
		return nil, 0, err
	}
	list := make([]Invoice, 0)
	err := r.db.Select(&list,
		"SELECT "+invoiceColumns+" FROM invoices "+where+" ORDER BY applied_at DESC LIMIT ? OFFSET ?",
		append(args, pageSize, (page-1)*pageSize)...)
	return list, total, err
}

// IssueInvoice CAS 完成开票: 仅 pending → issued, 同时写入税务号码与 PDF。
// 返回 false 表示发票不存在或已不在 pending(重复提交/并发审核安全)。
func (r *Repository) IssueInvoice(id int64, taxInvoiceNo, filename string, pdf []byte) (bool, error) {
	res, err := r.db.Exec(
		`UPDATE invoices SET status='issued', tax_invoice_no=?, pdf_blob=?, pdf_filename=?, issued_at=NOW()
		WHERE id=? AND status='pending'`,
		taxInvoiceNo, pdf, filename, id)
	if err != nil {
		return false, err
	}
	affected, _ := res.RowsAffected()
	return affected == 1, nil
}

// RejectInvoice CAS 驳回: 仅 pending → rejected。
func (r *Repository) RejectInvoice(id int64, reason string) (bool, error) {
	res, err := r.db.Exec(
		"UPDATE invoices SET status='rejected', reject_reason=? WHERE id=? AND status='pending'",
		reason, id)
	if err != nil {
		return false, err
	}
	affected, _ := res.RowsAffected()
	return affected == 1, nil
}

func normalizePage(page, pageSize int) (int, int) {
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}
	return page, pageSize
}
