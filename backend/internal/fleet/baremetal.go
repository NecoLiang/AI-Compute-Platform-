package fleet

// 整机(bare_metal)托管的租约侧流转(T-089, docs/25 §25.7.4)。命令模板见 baremetal_cmds.go。

import (
	"github.com/jmoiron/sqlx"
)

// markBareMetalReimageTx 整机租约结束(买家曾持有 root): 节点置重装闸门 → needs_reimage, 记审计。
// 闸门在供给方提交重装确认(新主机指纹)前挡住一切回到 verified 的路径(见 repository.markVerified)。
// 调用方已锁租约与节点行; 重复调用幂等(闸门时间与旧指纹只在第一次记录)。
func (s *Service) markBareMetalReimageTx(tx *sqlx.Tx, l *Lease, reason string) error {
	if err := s.repo.markNeedsReimage(tx, l.NodeID); err != nil {
		return err
	}
	return s.repo.audit(tx, 0, "node.needs_reimage", "supplier_node", l.NodeID, nil,
		map[string]any{"lease_no": l.LeaseNo, "reason": reason}, "")
}
