package fleet

// restart 续租的宽限期内原节点恢复(docs/25 §25.7.2, T-088), 开通步骤 ④ 的分支:
//
//	原容器在且卡集合与本次预留一致 → docker start(已在运行则不动), 主机密钥、端口、数据卷均不变;
//	原容器在但卡变了(原卡宽限期内被转租) → docker commit 为本租约提交镜像, 以新卡重建(挂载原数据卷);
//	原容器不在(上次重建中途失败) → 有提交镜像用提交镜像重建, 否则用基础镜像新建(数据卷仍沿用)。
//
// 同一节点上同一租约的任务串行执行(claimNextFor), 宽限期 stop 不会与本步骤并发。

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
)

func (p *provisionRun) resumeContainer(ctx context.Context, spec leaseContainerSpec) error {
	l := p.l
	cmd, err := containerStateCommand(l.LeaseNo)
	if err != nil {
		return err
	}
	out, err := runRemote(ctx, p.client, cmd, cmdTimeout)
	if err != nil {
		return fmt.Errorf("查询原容器状态失败: %v", errText(err))
	}
	st, err := parseContainerState(out)
	if err != nil {
		return err
	}
	switch {
	case st.Exists && sameGPUSet(st.GPUs, spec.UUIDs):
		p.out.Resume = "started"
		if st.Running {
			return nil
		}
		cmd, err := startContainerCommand(l.LeaseNo)
		if err != nil {
			return err
		}
		out, err := runRemote(ctx, p.client, cmd, 2*cmdTimeout)
		if err != nil || !strings.Contains(out, "@@started") {
			return fmt.Errorf("启动原容器失败: %v", errText(err))
		}
		return nil
	case st.Exists:
		ref, err := commitRefOf(l.LeaseNo, time.Now().Unix())
		if err != nil {
			return err
		}
		cmd, err := commitContainerCommand(l.LeaseNo, ref)
		if err != nil {
			return err
		}
		out, err := runRemote(ctx, p.client, cmd, 6*cmdTimeout)
		if err != nil || !strings.Contains(out, "@@committed") {
			return fmt.Errorf("提交原容器失败: %v", errText(err))
		}
		prev := deref(l.CommitImage)
		if err := p.recordCommitImage(ref); err != nil {
			return err
		}
		spec.Image = ref
		if err := p.createContainer(ctx, spec); err != nil {
			return err
		}
		p.out.Resume = "recreated"
		if prev != "" && prev != ref {
			p.removeCommitImage(ctx, prev)
		}
		return nil
	default:
		if l.CommitImage != nil && isLeaseCommitRef(l.LeaseNo, *l.CommitImage) {
			spec.Image = *l.CommitImage
			p.out.Resume = "recreated"
		} else {
			p.out.Resume = "fresh"
		}
		return p.createContainer(ctx, spec)
	}
}

// recordCommitImage 先落库再重建: 重建中途失败时下次重试(原容器已被 docker rm)仍能从提交镜像恢复, purge 也能据此删除镜像。
func (p *provisionRun) recordCommitImage(ref string) error {
	ok, err := affected(p.s.repo.db.Exec(`UPDATE leases SET commit_image=? WHERE id=? AND state='provisioning' AND node_id=?`,
		ref, p.l.ID, p.n.ID))
	if err != nil {
		return err
	}
	if !ok {
		return errTaskAborted{fmt.Sprintf("租约 %s 已不在开通中, 停止恢复(提交镜像 %s 由 purge 或人工清理)", p.l.LeaseNo, ref)}
	}
	p.l.CommitImage = &ref
	return nil
}

// removeCommitImage 旧提交镜像已被新提交取代: 尽力删除, 失败只记日志(不影响交付, purge 时不再追踪它)。
func (p *provisionRun) removeCommitImage(ctx context.Context, ref string) {
	cmd, err := removeCommitImageCommand(ref)
	if err == nil {
		_, err = runRemote(ctx, p.client, cmd, 2*cmdTimeout)
	}
	if err != nil {
		slog.Warn("删除旧提交镜像失败, 需人工清理", "lease_no", p.l.LeaseNo, "node_id", p.n.ID, "image", ref, "error", errText(err))
	}
}

func sameGPUSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}
