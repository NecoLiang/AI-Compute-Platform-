package fleet

import (
	"crypto/rsa"
	"database/sql"
	"errors"
	"net"
	"strings"
	"time"
	"unicode/utf8"

	"tokenfactory/internal/compute"
	"tokenfactory/pkg/errcode"

	"golang.org/x/crypto/ssh"
)

// 买家连接与运营检索(docs/25 §25.6.1/§25.10, T-086): 买家公钥管理、托管订单连接信息与会话记录、
// 运营租约/会话检索。正式契约见 docs/api/managed-rental-api.md。

const (
	maxBuyerKeys     = 10
	maxBuyerKeyName  = 64
	maxPublicKeyLen  = 1024 // buyer_ssh_keys.public_key VARCHAR(1024)
	minRSABits       = 3072
	defaultKeyName   = "SSH 公钥"
	maxPageSize      = 100
	defaultPageSize  = 20
	authMethodPubkey = "publickey"
	authMethodPasswd = "password"
)

// BuyerKey buyer_ssh_keys 中未吊销的一行(公钥按解析结果重新序列化, 不含注释)。
type BuyerKey struct {
	ID          int64     `db:"id" json:"id"`
	Name        string    `db:"name" json:"name"`
	KeyType     string    `db:"-" json:"key_type"`
	PublicKey   string    `db:"public_key" json:"public_key"`
	Fingerprint string    `db:"fingerprint" json:"fingerprint"`
	CreatedAt   time.Time `db:"created_at" json:"created_at"`
}

// parsedKey 校验后的买家公钥。
type parsedKey struct {
	canonical   string
	fingerprint string
	keyType     string
	comment     string
}

// parseBuyerKey 只接受一行 authorized_keys 格式公钥: ssh-ed25519 / ecdsa-sha2-nistp{256,384,521} / ssh-rsa(≥3072 位)。
// 不接受带选项的行、证书、安全密钥(sk-*)、DSA 与多行输入。
func parseBuyerKey(line string) (*parsedKey, error) {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil, errcode.New(errcode.ParamInvalid, "public_key 不能为空")
	}
	if strings.ContainsAny(line, "\r\n") {
		return nil, errcode.New(errcode.ParamInvalid, "一次只能添加一把公钥(public_key 不能包含换行)")
	}
	pub, comment, options, rest, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		return nil, errcode.New(errcode.ParamInvalid, "公钥格式无法识别, 请粘贴 ~/.ssh/id_ed25519.pub 这类 .pub 文件的完整一行")
	}
	if len(options) > 0 {
		return nil, errcode.New(errcode.ParamInvalid, "不支持带选项(如 from=/command=)的公钥行")
	}
	if len(strings.TrimSpace(string(rest))) > 0 {
		return nil, errcode.New(errcode.ParamInvalid, "一次只能添加一把公钥")
	}
	switch t := pub.Type(); t {
	case ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521:
	case ssh.KeyAlgoRSA:
		cpk, ok := pub.(ssh.CryptoPublicKey)
		if !ok {
			return nil, errcode.New(errcode.ParamInvalid, "RSA 公钥无法解析")
		}
		rk, ok := cpk.CryptoPublicKey().(*rsa.PublicKey)
		if !ok || rk.N.BitLen() < minRSABits {
			return nil, errcode.New(errcode.ParamInvalid, "RSA 公钥至少 3072 位, 建议改用 ed25519(ssh-keygen -t ed25519)")
		}
	default:
		return nil, errcode.New(errcode.ParamInvalid, "不支持的公钥类型 "+t+", 仅支持 ssh-ed25519、ecdsa-sha2-nistp256/384/521、ssh-rsa(≥3072 位)")
	}
	canonical := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))
	if len(canonical) > maxPublicKeyLen {
		return nil, errcode.New(errcode.ParamInvalid, "公钥过长, 请改用 ed25519 或 3072/4096 位 RSA")
	}
	return &parsedKey{canonical: canonical, fingerprint: ssh.FingerprintSHA256(pub), keyType: pub.Type(),
		comment: strings.TrimSpace(comment)}, nil
}

func keyTypeOf(line string) string {
	t, _, _ := strings.Cut(line, " ")
	return t
}

// keyName 显式名称超长报错; 缺省时取公钥注释(截断到 64 字), 再缺省为 "SSH 公钥"。
func keyName(name, comment string) (string, error) {
	name = strings.TrimSpace(name)
	if name != "" {
		if utf8.RuneCountInString(name) > maxBuyerKeyName {
			return "", errcode.New(errcode.ParamInvalid, "name 不能超过 64 个字符")
		}
		return name, nil
	}
	if comment == "" || !utf8.ValidString(comment) {
		return defaultKeyName, nil
	}
	r := []rune(comment)
	if len(r) > maxBuyerKeyName {
		r = r[:maxBuyerKeyName]
	}
	return string(r), nil
}

// ListBuyerKeys 本人未吊销的公钥, 按添加时间倒序。
func (s *Service) ListBuyerKeys(userID int64) ([]BuyerKey, error) {
	list := []BuyerKey{}
	if err := s.repo.db.Select(&list, `SELECT id, name, public_key, fingerprint, created_at FROM buyer_ssh_keys
		WHERE user_id=? AND revoked_at IS NULL ORDER BY created_at DESC, id DESC`, userID); err != nil {
		return nil, err
	}
	for i := range list {
		list[i].KeyType = keyTypeOf(list[i].PublicKey)
	}
	return list, nil
}

// AddBuyerKey 添加公钥: 每人最多 10 把(未吊销), 同一把重复添加 40900; 曾删除过的同一把重新启用。
// 先锁用户行把同一买家的并发添加串行化, 上限计数不会被并发突破。新增公钥只扩大放行范围, 不需要刷新撤销游标。
func (s *Service) AddBuyerKey(userID int64, name, publicKey, ip string) (*BuyerKey, error) {
	pk, err := parseBuyerKey(publicKey)
	if err != nil {
		return nil, err
	}
	nm, err := keyName(name, pk.comment)
	if err != nil {
		return nil, err
	}
	tx, err := s.repo.db.Beginx()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var uid int64
	if err := tx.Get(&uid, `SELECT id FROM users WHERE id=? FOR UPDATE`, userID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errcode.New(errcode.NotFound, "用户不存在")
		}
		return nil, err
	}
	var existing struct {
		ID      int64      `db:"id"`
		Revoked *time.Time `db:"revoked_at"`
	}
	err = tx.Get(&existing, `SELECT id, revoked_at FROM buyer_ssh_keys WHERE user_id=? AND fingerprint=? FOR UPDATE`, userID, pk.fingerprint)
	found := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if found && existing.Revoked == nil {
		return nil, errcode.New(errcode.Conflict, "该公钥已添加")
	}
	var n int
	if err := tx.Get(&n, `SELECT COUNT(*) FROM buyer_ssh_keys WHERE user_id=? AND revoked_at IS NULL`, userID); err != nil {
		return nil, err
	}
	if n >= maxBuyerKeys {
		return nil, errcode.New(errcode.Conflict, "每个账户最多 10 把公钥, 请先删除不用的公钥")
	}
	id := existing.ID
	if found {
		if _, err := tx.Exec(`UPDATE buyer_ssh_keys SET name=?, public_key=?, revoked_at=NULL, created_at=NOW() WHERE id=?`,
			nm, pk.canonical, id); err != nil {
			return nil, err
		}
	} else {
		res, err := tx.Exec(`INSERT INTO buyer_ssh_keys (user_id, name, public_key, fingerprint) VALUES (?,?,?,?)`,
			userID, nm, pk.canonical, pk.fingerprint)
		if err != nil {
			if isDuplicate(err) {
				return nil, errcode.New(errcode.Conflict, "该公钥已添加")
			}
			return nil, err
		}
		if id, err = res.LastInsertId(); err != nil {
			return nil, err
		}
	}
	if err := s.repo.audit(tx, userID, "buyer_ssh_key.add", "buyer_ssh_key", id, nil,
		map[string]string{"name": nm, "fingerprint": pk.fingerprint}, ip); err != nil {
		return nil, err
	}
	var k BuyerKey
	if err := tx.Get(&k, `SELECT id, name, public_key, fingerprint, created_at FROM buyer_ssh_keys WHERE id=?`, id); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	k.KeyType = keyTypeOf(k.PublicKey)
	return &k, nil
}

// DeleteBuyerKey 删除(吊销)公钥, 同一事务内刷新买家名下在租租约的撤销游标: 网关在下一次轮询(≤ 5s + 2s 滞后)
// 清掉按该公钥缓存的放行。已建立的会话不受影响(网关插件无法切断已建立的管道, docs/25 §25.6.3 S2 口径)。
func (s *Service) DeleteBuyerKey(userID, keyID int64, ip string) error {
	tx, err := s.repo.db.Beginx()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var fp string
	err = tx.Get(&fp, `SELECT fingerprint FROM buyer_ssh_keys WHERE id=? AND user_id=? AND revoked_at IS NULL FOR UPDATE`, keyID, userID)
	if errors.Is(err, sql.ErrNoRows) {
		return errcode.New(errcode.NotFound, "公钥不存在")
	}
	if err != nil {
		return err
	}
	ok, err := affected(tx.Exec(`UPDATE buyer_ssh_keys SET revoked_at=NOW() WHERE id=? AND revoked_at IS NULL`, keyID))
	if err != nil {
		return err
	}
	if !ok {
		return errcode.New(errcode.NotFound, "公钥不存在")
	}
	if err := s.repo.touchBuyerLeases(tx, userID); err != nil {
		return err
	}
	if err := s.repo.audit(tx, userID, "buyer_ssh_key.revoke", "buyer_ssh_key", keyID,
		map[string]string{"fingerprint": fp}, nil, ip); err != nil {
		return err
	}
	return tx.Commit()
}

// OrderAccess GET /orders/:id/access 的连接信息。不含节点地址与容器端口。
type OrderAccess struct {
	GatewayHost string     `json:"gateway_host"`
	GatewayPort int        `json:"gateway_port"`
	Login       string     `json:"login"`
	LeaseState  string     `json:"lease_state"`
	AccessUntil *time.Time `json:"access_until"`
	GraceUntil  *time.Time `json:"grace_until"`
	AuthMethods []string   `json:"auth_methods"`
}

// buyerManagedLease 买家本人托管订单的租约。不存在 40400, 非本人 40300(先于履约模式判断, 不泄露他人订单类型),
// 人工订单 40400。
func (s *Service) buyerManagedLease(userID int64, orderNo string) (*compute.Order, *Lease, error) {
	o, err := s.products.OrderForAccess(orderNo)
	if err != nil {
		return nil, nil, err
	}
	if o == nil {
		return nil, nil, errcode.New(errcode.NotFound, "订单不存在")
	}
	if o.BuyerID != userID {
		return nil, nil, errcode.New(errcode.Forbidden, "无权查看该订单")
	}
	if o.FulfillmentMode != compute.FulfillmentManaged {
		return nil, nil, errcode.New(errcode.NotFound, "该订单为人工交付, 没有网关连接信息")
	}
	l, err := s.repo.getLeaseByOrder(s.repo.db, o.ID)
	if err != nil {
		return nil, nil, err
	}
	if l == nil {
		return nil, nil, errcode.New(errcode.NotFound, "订单尚无租约")
	}
	return o, l, nil
}

// OrderAccess 托管订单连接信息。登录名即租约号, 换节点/续租不变; 能否登录以 lease_state=active 且未过 access_until 为准。
func (s *Service) OrderAccess(userID int64, orderNo string) (*OrderAccess, error) {
	_, l, err := s.buyerManagedLease(userID, orderNo)
	if err != nil {
		return nil, err
	}
	return &OrderAccess{GatewayHost: s.gw.PublicHost, GatewayPort: s.gw.PublicPort, Login: l.LeaseNo, LeaseState: l.State,
		AccessUntil: l.AccessUntil, GraceUntil: l.GraceUntil, AuthMethods: []string{authMethodPubkey, authMethodPasswd}}, nil
}

// AccessSession access_sessions 一行(买家与运营共用; buyer_id 仅运营检索返回)。
type AccessSession struct {
	ID             int64      `db:"id" json:"id"`
	LeaseNo        string     `db:"lease_no" json:"lease_no"`
	BuyerID        int64      `db:"buyer_id" json:"buyer_id,omitempty"`
	RemoteIP       string     `db:"remote_ip" json:"remote_ip"`
	AuthMethod     string     `db:"auth_method" json:"auth_method"`
	KeyFingerprint *string    `db:"key_fingerprint" json:"key_fingerprint"`
	KeyName        *string    `db:"key_name" json:"key_name"`
	StartedAt      time.Time  `db:"started_at" json:"started_at"`
	EndedAt        *time.Time `db:"ended_at" json:"ended_at"`
	BytesUp        int64      `db:"bytes_up" json:"bytes_up"`
	BytesDown      int64      `db:"bytes_down" json:"bytes_down"`
	EndReason      *string    `db:"end_reason" json:"end_reason"`
}

// SessionFilter 会话检索条件。
type SessionFilter struct {
	LeaseID  int64
	IP       string
	Page     int
	PageSize int
}

func normPage(page, size int) (int, int) {
	if page < 1 {
		page = 1
	}
	if size < 1 || size > maxPageSize {
		size = defaultPageSize
	}
	return page, size
}

// listSessions key_name 取买家当时登记该公钥的名称(含已删除的公钥, 便于审计)。
func (r *Repository) listSessions(f SessionFilter, withBuyer bool) ([]AccessSession, int64, error) {
	where, args := []string{"1=1"}, []any{}
	if f.LeaseID > 0 {
		where, args = append(where, "s.lease_id=?"), append(args, f.LeaseID)
	}
	if f.IP != "" {
		where, args = append(where, "s.remote_ip=?"), append(args, f.IP)
	}
	w := " WHERE " + strings.Join(where, " AND ")
	var total int64
	if err := r.db.Get(&total, `SELECT COUNT(*) FROM access_sessions s`+w, args...); err != nil {
		return nil, 0, err
	}
	buyer := "0"
	if withBuyer {
		buyer = "l.buyer_id"
	}
	list := []AccessSession{}
	err := r.db.Select(&list, `SELECT s.id, l.lease_no, `+buyer+` buyer_id, s.remote_ip, s.auth_method, s.key_fingerprint,
		(SELECT k.name FROM buyer_ssh_keys k WHERE k.user_id=l.buyer_id AND k.fingerprint=s.key_fingerprint LIMIT 1) key_name,
		s.started_at, s.ended_at, s.bytes_up, s.bytes_down, s.end_reason
		FROM access_sessions s JOIN leases l ON l.id=s.lease_id`+w+` ORDER BY s.started_at DESC, s.id DESC LIMIT ? OFFSET ?`,
		append(args, f.PageSize, (f.Page-1)*f.PageSize)...)
	return list, total, err
}

// OrderSessions 买家本人托管订单的会话记录(保留 180 天)。
func (s *Service) OrderSessions(userID int64, orderNo string, page, pageSize int) ([]AccessSession, int64, int, int, error) {
	_, l, err := s.buyerManagedLease(userID, orderNo)
	if err != nil {
		return nil, 0, 0, 0, err
	}
	page, pageSize = normPage(page, pageSize)
	list, total, err := s.repo.listSessions(SessionFilter{LeaseID: l.ID, Page: page, PageSize: pageSize}, false)
	return list, total, page, pageSize, err
}

// AdminSessions 运营会话检索: lease_no 与 ip 可单独或组合使用, 均缺省为全部。
func (s *Service) AdminSessions(leaseNo, ip string, page, pageSize int) ([]AccessSession, int64, int, int, error) {
	page, pageSize = normPage(page, pageSize)
	f := SessionFilter{Page: page, PageSize: pageSize}
	if leaseNo != "" {
		if !ValidLeaseNo(leaseNo) {
			return nil, 0, 0, 0, errcode.New(errcode.ParamInvalid, "lease_no 格式非法(l- + 8 位小写字母/数字 2-7)")
		}
		l, err := s.repo.getLeaseByNo(s.repo.db, leaseNo)
		if err != nil {
			return nil, 0, 0, 0, err
		}
		if l == nil {
			return []AccessSession{}, 0, page, pageSize, nil
		}
		f.LeaseID = l.ID
	}
	if ip != "" {
		p := net.ParseIP(ip)
		if p == nil {
			return nil, 0, 0, 0, errcode.New(errcode.ParamInvalid, "ip 非法")
		}
		f.IP = p.String()
	}
	list, total, err := s.repo.listSessions(f, true)
	return list, total, page, pageSize, err
}

var leaseStates = []string{LeaseReserved, LeasePending, LeaseProvisioning, LeaseActive, LeaseParked, LeaseSuspended,
	LeaseGrace, LeasePurging, LeasePurged, LeaseFailed, LeaseCancelled}

// AdminLeases 运营租约检索: state/node_id/buyer_id 可组合, 均缺省为全部, 按 id 倒序。
func (s *Service) AdminLeases(state string, nodeID, buyerID int64, page, pageSize int) ([]Lease, int64, int, int, error) {
	if state != "" && !containsStr(leaseStates, state) {
		return nil, 0, 0, 0, errcode.New(errcode.ParamInvalid, "state 仅支持 "+strings.Join(leaseStates, "/"))
	}
	if nodeID < 0 || buyerID < 0 {
		return nil, 0, 0, 0, errcode.New(errcode.ParamInvalid, "node_id/buyer_id 非法")
	}
	page, pageSize = normPage(page, pageSize)
	list, total, err := s.repo.listLeases(LeaseFilter{State: state, NodeID: nodeID, BuyerID: buyerID, Page: page, PageSize: pageSize})
	if list == nil {
		list = []Lease{}
	}
	return list, total, page, pageSize, err
}
