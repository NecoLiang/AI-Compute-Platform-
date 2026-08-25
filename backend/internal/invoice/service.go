package invoice

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
)

// ===== 状态与常量 =====

const (
	StatusPending    = "pending"
	StatusIssued     = "issued"
	StatusRejected   = "rejected"
	StatusRedFlushed = "red_flushed" // 预留, 本迭代不实现红冲流程
)

const (
	InvoiceTypeVATSpecial = "vat_special" // 增值税专用发票
	TitleTypeEnterprise   = "enterprise"
	TitleTypePersonal     = "personal" // 预留
)

// MaxInvoicePDFBytes 上传 PDF 上限 5MB, 与 MEDIUMBLOB 实际使用场景匹配。
const MaxInvoicePDFBytes = 5 << 20

// ===== 纯函数校验(表驱动单测覆盖, 不触 DB) =====

var taxNoPattern = regexp.MustCompile(`^[0-9A-Z]{15}$|^[0-9A-Z]{18}$|^[0-9A-Z]{20}$`)

var bankAccountPattern = regexp.MustCompile(`^\d{8,32}$`)

// ValidateTaxNo 纳税人识别号: 15/18/20 位大写字母数字(统一社会信用代码为 18 位)。
func ValidateTaxNo(taxNo string) bool {
	return taxNoPattern.MatchString(taxNo)
}

// CanInvoiceOrder 已付款订单才可开票: paid/provisioning/active/completed。
// 未支付、已取消、退款中/已退款、冻结均不可开。
func CanInvoiceOrder(status string) bool {
	switch status {
	case "paid", "provisioning", "active", "completed":
		return true
	}
	return false
}

// SumInvoiceAmountFen 开票金额 = 关联订单实付合计(分)。
func SumInvoiceAmountFen(orders []BillableOrder) int64 {
	var sum int64
	for _, o := range orders {
		sum += o.TotalAmount
	}
	return sum
}

// NextInvoiceNoFromMax 由当前最大编号生成下一张: 同年序号 +1, 无记录从 0001 起。
// maxNo 为空或格式异常(跨年数据)时安全回落到 0001。
func NextInvoiceNoFromMax(prefix, maxNo string) string {
	seq := 0
	if strings.HasPrefix(maxNo, prefix) {
		fmt.Sscanf(maxNo[len(prefix):], "%d", &seq)
	}
	return fmt.Sprintf("%s%04d", prefix, seq+1)
}

// IsPDF 检查 %PDF- magic bytes, 防止把任意文件当发票 PDF 存库。
func IsPDF(b []byte) bool {
	return len(b) >= 5 && string(b[:5]) == "%PDF-"
}

// ===== 请求模型 =====

type SaveTitleReq struct {
	CompanyName string  `json:"company_name"`
	TaxNo       string  `json:"tax_no"`
	BankName    string  `json:"bank_name"`
	BankAccount string  `json:"bank_account"`
	Address     *string `json:"address"`
	Phone       *string `json:"phone"`
}

// validate 面向用户的中文校验错误, 与 compute 包 containsCJK → ParamInvalid 的约定一致。
func (r *SaveTitleReq) validate() error {
	r.CompanyName = strings.TrimSpace(r.CompanyName)
	r.TaxNo = strings.ToUpper(strings.TrimSpace(r.TaxNo))
	r.BankName = strings.TrimSpace(r.BankName)
	r.BankAccount = strings.TrimSpace(r.BankAccount)
	if r.CompanyName == "" {
		return fmt.Errorf("企业名称不能为空")
	}
	if !ValidateTaxNo(r.TaxNo) {
		return fmt.Errorf("纳税人识别号格式不正确(15/18/20 位大写字母或数字)")
	}
	if r.BankName == "" {
		return fmt.Errorf("开户行不能为空")
	}
	if r.BankAccount == "" {
		return fmt.Errorf("银行账号不能为空")
	}
	if !bankAccountPattern.MatchString(r.BankAccount) {
		return fmt.Errorf("银行账号为 8-32 位数字")
	}
	return nil
}

// ===== Service =====

type Service struct {
	repo *Repository
	db   *sqlx.DB
}

func NewService(repo *Repository, db *sqlx.DB) *Service {
	return &Service{repo: repo, db: db}
}

// ---- Title ----

func (s *Service) GetTitle(buyerID int64) (*InvoiceTitle, error) {
	return s.repo.GetTitle(buyerID)
}

func (s *Service) SaveTitle(buyerID int64, req SaveTitleReq) (*InvoiceTitle, error) {
	if err := req.validate(); err != nil {
		return nil, err
	}
	t := &InvoiceTitle{
		BuyerID: buyerID, TitleType: TitleTypeEnterprise,
		CompanyName: req.CompanyName, TaxNo: req.TaxNo,
		BankName: req.BankName, BankAccount: req.BankAccount,
		Address: req.Address, Phone: req.Phone,
	}
	if err := s.repo.UpsertTitle(t); err != nil {
		return nil, err
	}
	return s.repo.GetTitle(buyerID)
}

// ListBillableOrders 可开票订单(申请弹窗数据源)。
func (s *Service) ListBillableOrders(buyerID int64) ([]BillableOrder, error) {
	return s.repo.ListBillableOrders(buyerID)
}

// ---- Apply ----

// Apply 申请开票: 单事务内锁订单 → 校验归属与可开票状态 → 合计金额 →
// 抬头快照 → 生成编号 → 写发票与关联。invoice_orders.uq_order 兜底并发重复申请。
func (s *Service) Apply(buyerID int64, orderNos []string) (*Invoice, error) {
	if len(orderNos) == 0 {
		return nil, fmt.Errorf("请选择需要开票的订单")
	}
	if len(orderNos) > 50 {
		return nil, fmt.Errorf("单张发票最多合并 50 个订单")
	}
	seen := make(map[string]bool, len(orderNos))
	for _, no := range orderNos {
		no = strings.TrimSpace(no)
		if no == "" || seen[no] {
			return nil, fmt.Errorf("订单号无效或重复")
		}
		seen[no] = true
	}

	title, err := s.repo.GetTitle(buyerID)
	if err != nil {
		return nil, err
	}
	if title == nil {
		return nil, fmt.Errorf("请先完善开票信息")
	}

	tx, err := s.db.Beginx()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	orders, err := s.repo.LockBillableOrdersByNos(tx, buyerID, orderNos)
	if err != nil {
		return nil, err
	}
	if len(orders) != len(orderNos) {
		return nil, fmt.Errorf("存在不可开票订单: 未支付、已退款或已在其他发票中申请")
	}
	orderIDs := make([]int64, len(orders))
	for i, o := range orders {
		if !CanInvoiceOrder(o.Status) {
			return nil, fmt.Errorf("订单 %s 当前状态不可开票", o.OrderNo)
		}
		orderIDs[i] = o.OrderID
	}

	inv := &Invoice{
		BuyerID:     buyerID,
		CompanyName: title.CompanyName, TaxNo: title.TaxNo,
		BankName: title.BankName, BankAccount: title.BankAccount,
		AmountFen:   SumInvoiceAmountFen(orders),
		InvoiceType: InvoiceTypeVATSpecial,
		Status:      StatusPending,
	}
	if inv.InvoiceNo, err = s.repo.NextInvoiceNo(tx, time.Now().Year()); err != nil {
		return nil, err
	}
	inv.ID, err = s.repo.CreateInvoiceTx(tx, inv)
	if err != nil {
		return nil, err
	}
	if err = s.repo.LinkOrdersTx(tx, inv.ID, orderIDs); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	inv.AppliedAt = time.Now()
	return inv, nil
}

// ---- List / Download ----

func (s *Service) ListInvoices(buyerID int64, page, pageSize int) ([]Invoice, int64, error) {
	return s.repo.ListInvoices(buyerID, page, pageSize)
}

// Download 下载发票 PDF: 归属校验 + 已开票才有文件。
func (s *Service) Download(buyerID int64, invoiceNo string) ([]byte, string, error) {
	pdf, filename, err := s.repo.GetInvoicePDF(buyerID, invoiceNo)
	if err != nil {
		return nil, "", err
	}
	if len(pdf) == 0 {
		return nil, "", fmt.Errorf("invoice not found")
	}
	name := invoiceNo + ".pdf"
	if filename != nil && *filename != "" {
		name = *filename
	}
	return pdf, name, nil
}

// ---- Admin ----

func (s *Service) ListAllInvoices(status string, page, pageSize int) ([]Invoice, int64, error) {
	if status != "" && status != StatusPending && status != StatusIssued &&
		status != StatusRejected && status != StatusRedFlushed {
		return nil, 0, fmt.Errorf("invalid status")
	}
	return s.repo.ListAllInvoices(status, page, pageSize)
}

// Issue 运营完成开票: 登记税务号码并写入 PDF。taxInvoiceNo 可为空(占位票),
// 但 PDF 必须提供, 否则买家侧永远没有可下载文件。
func (s *Service) Issue(id int64, taxInvoiceNo, filename string, pdf []byte) error {
	if len(pdf) == 0 || len(pdf) > MaxInvoicePDFBytes {
		return fmt.Errorf("PDF 大小必须在 5MB 以内")
	}
	if !IsPDF(pdf) {
		return fmt.Errorf("文件不是有效的 PDF")
	}
	ok, err := s.repo.IssueInvoice(id, strings.TrimSpace(taxInvoiceNo), filename, pdf)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("invoice not found")
	}
	return nil
}

func (s *Service) Reject(id int64, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return fmt.Errorf("请填写驳回原因")
	}
	ok, err := s.repo.RejectInvoice(id, reason)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("invoice not found")
	}
	return nil
}
