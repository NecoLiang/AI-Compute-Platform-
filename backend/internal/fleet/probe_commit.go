package fleet

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jmoiron/sqlx"
)

// passConflict 各项通过但无法上架(指纹/卡 UUID 已被其他节点占用), 转失败路径。
type passConflict struct{ check, msg string }

func (e passConflict) Error() string { return e.msg }

// commitProbe 把一次探活的结论落库: 报告 + 节点状态 + 卡槽 + 派生库存, 然后通知与刷新健康度。
func (s *Service) commitProbe(p *probeRun) (*ProbeOutcome, error) {
	kind := "verify"
	if p.mode == ProbePortRecheck {
		kind = "port_recheck"
	}
	out := &ProbeOutcome{Mode: p.mode}
	n := p.n

	if p.mismatch != "" {
		if stale, err := s.probeStale(p); err != nil || stale {
			out.Discarded = stale
			return out, err
		}
		s.handleKeyMismatch(n.ID, p.gen, *n.HostKeyFingerprint, p.mismatch)
		id, err := s.repo.insertReport(s.repo.db, n.ID, kind, false, p.list(), nil)
		if err != nil {
			return nil, err
		}
		out.ReportID, out.Failed = id, p.failed()
		return out, nil
	}

	if p.await {
		err := s.inTx(func(tx *sqlx.Tx) error {
			cur, err := s.repo.lockNode(tx, n.ID)
			if err != nil {
				return err
			}
			if cur == nil || cur.Generation != p.gen {
				out.Discarded = true
				return nil
			}
			id, err := s.repo.insertReport(tx, n.ID, kind, false, p.list(), nil)
			if err != nil {
				return err
			}
			out.ReportID = id
			return s.repo.markAwaitingFingerprint(tx, n.ID, p.gen)
		})
		if err != nil {
			return nil, err
		}
		if out.Discarded {
			slog.Info("探活结论作废: 节点代次已变化", "node_id", n.ID)
			return out, nil
		}
		out.AwaitingHostKey, out.Failed = p.fp, p.failed()
		s.notify(n.SupplierID, "请确认节点主机指纹",
			fmt.Sprintf("平台首次连接节点「%s」, 观测到 SSH 主机指纹 %s。请在机器上执行 %s 核对一致后, 在节点页确认指纹, 探活将自动继续。",
				n.NodeName, p.fp, fingerprintCheckCommand), n.ID)
		return out, nil
	}

	var inv *Inventory
	if p.mode == ProbeFull {
		p.inv.DockerVersion = p.docker.ServerVersion
		inv = &p.inv
	}

	if len(p.failed()) == 0 {
		if p.mode == ProbePortRecheck {
			if stale, err := s.probeStale(p); err != nil || stale {
				out.Discarded = stale
				return out, err
			}
			id, err := s.repo.insertReport(s.repo.db, n.ID, kind, true, p.list(), nil)
			if err != nil {
				return nil, err
			}
			out.Pass, out.ReportID = true, id
			return out, nil
		}
		var productID int64
		err := s.inTx(func(tx *sqlx.Tx) error {
			cur, err := s.repo.lockNode(tx, n.ID)
			if err != nil {
				return err
			}
			if cur == nil || cur.Generation != p.gen {
				out.Discarded = true
				return nil
			}
			productID = cur.ProductID
			if other, err := s.repo.fingerprintClaimedBy(tx, p.fp, n.ID); err != nil {
				return err
			} else if other > 0 {
				return passConflict{"P2", fmt.Sprintf("该主机(指纹 %s)已作为节点 #%d 上架, 同一台机器不能重复上架", p.fp, other)}
			}
			ok, err := s.repo.markVerified(tx, n.ID, guardOf(n), p.gen, p.fp, inv)
			if isDuplicate(err) {
				return passConflict{"P2", fmt.Sprintf("该主机(指纹 %s)已被其他节点占用, 同一台机器不能重复上架", p.fp)}
			}
			if err != nil {
				return err
			}
			if !ok { // 探活期间连接信息/指纹/代次变化或节点不在 verifying/verified: 结论作废
				out.Discarded = true
				return nil
			}
			if err := s.repo.syncSlots(tx, n.ID, inv); isDuplicate(err) {
				return passConflict{"P4", "卡 UUID 已被其他节点登记, 同一张卡不能重复上架"}
			} else if err != nil {
				return err
			}
			id, err := s.repo.insertReport(tx, n.ID, kind, true, p.list(), inv)
			if err != nil {
				return err
			}
			out.ReportID = id
			if err := s.repo.audit(tx, 0, "node.verified", "supplier_node", n.ID,
				map[string]any{"verify_status": cur.VerifyStatus},
				map[string]any{"verify_status": VerifyVerified, "report_id": id, "faulty": inv.Faulty}, ""); err != nil {
				return err
			}
			return s.recomputeStockTx(tx, cur.ProductID)
		})
		var pc passConflict
		switch {
		case errors.As(err, &pc):
			p.set(pc.check, false, pc.msg)
		case err != nil:
			return nil, err
		case out.Discarded:
			slog.Info("探活结论作废: 连接信息已修改或节点状态已变化", "node_id", n.ID)
			return out, nil
		default:
			out.Pass = true
			n.ProductID = productID
			s.refreshHealth(n)
			if p.trigger != "periodic" || n.VerifyStatus != VerifyVerified {
				msg := fmt.Sprintf("节点「%s」探活通过, 已可接单。", n.NodeName)
				if len(inv.Faulty) > 0 {
					msg += fmt.Sprintf("其中 %d 张卡未通过健康/压测检查, 已标记故障不计入可售。", len(inv.Faulty))
				}
				s.notify(n.SupplierID, "节点探活通过", msg, n.ID)
			}
			return out, nil
		}
	}

	return s.commitFail(p, kind, inv, out)
}

// commitFail 探活未通过: 写报告; 节点 → failed + offline(释放指纹占用)。
// 例外: 周期补跑的完整探活仅因连不上(P1-P3)失败时只写报告, 在线状态交给轻量探活判定, 避免网络抖动把节点打回 failed。
func (s *Service) commitFail(p *probeRun, kind string, inv *Inventory, out *ProbeOutcome) (*ProbeOutcome, error) {
	n := p.n
	failed := p.failed()
	connOnly := p.trigger == "periodic" && len(failed) > 0 &&
		(failed[0] == "P1" || failed[0] == "P2" || failed[0] == "P3")
	var wasVerified bool
	err := s.inTx(func(tx *sqlx.Tx) error {
		cur, err := s.repo.lockNode(tx, n.ID)
		if err != nil {
			return err
		}
		if cur == nil || cur.Generation != p.gen {
			out.Discarded = true
			return nil
		}
		id, err := s.repo.insertReport(tx, n.ID, kind, false, p.list(), inv)
		if err != nil {
			return err
		}
		out.ReportID = id
		if connOnly {
			return nil
		}
		wasVerified = cur.VerifyStatus == VerifyVerified
		ok, err := s.repo.markFailed(tx, n.ID, guardOf(n), p.gen)
		if err != nil {
			return err
		}
		if !ok {
			return errDiscard
		}
		if wasVerified {
			if err := s.repo.audit(tx, 0, "node.verify_failed", "supplier_node", n.ID,
				map[string]any{"verify_status": VerifyVerified},
				map[string]any{"verify_status": VerifyFailed, "report_id": id, "failed_checks": failed}, ""); err != nil {
				return err
			}
		}
		return s.recomputeStockTx(tx, cur.ProductID)
	})
	if errors.Is(err, errDiscard) {
		out.Discarded, out.ReportID = true, 0
		err = nil
	}
	if err != nil {
		return nil, err
	}
	if out.Discarded {
		slog.Info("探活结论作废: 节点代次或连接信息已变化", "node_id", n.ID)
		return out, nil
	}
	out.Failed = failed
	s.refreshHealth(n)
	if p.trigger != "periodic" || wasVerified {
		what := "探活"
		if kind == "port_recheck" {
			what = "租约端口复检"
		}
		msg := fmt.Sprintf("节点「%s」%s未通过(%s), 请查看探活报告按提示处理后重新发起探活。", n.NodeName, what, strings.Join(failed, "、"))
		if wasVerified {
			msg = fmt.Sprintf("节点「%s」%s未通过(%s), 已停止接受新租约(存量租约不受影响)。请按报告处理后重新发起探活。", n.NodeName, what, strings.Join(failed, "、"))
		}
		s.notify(n.SupplierID, "节点"+what+"未通过", msg, n.ID)
	}
	return out, nil
}

// errDiscard 事务内发现结论已过时: 回滚(连同已写的报告)并按作废处理。
var errDiscard = errors.New("探活结论作废")

// probeStale 节点已删除或代次已变(停用/恢复/重装确认/改连接信息/确认指纹之后), 本次探活结论不再写回。
func (s *Service) probeStale(p *probeRun) (bool, error) {
	cur, err := s.repo.GetNode(p.n.ID)
	if err != nil {
		return false, err
	}
	return cur == nil || cur.Generation != p.gen, nil
}
