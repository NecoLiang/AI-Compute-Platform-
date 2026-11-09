-- T-089: 整机托管(bare_metal)租约结束后的重装闸门(docs/25 §25.7.4)。
-- reimage_required_at: 买家曾持有 root 的整机租约结束(到期/冻结后到期/退款)时写入; 非空期间节点不能被任何路径
--   (PUT /ssh、运营恢复、探活重试)改回可售, markVerified 亦拒绝写回。供给方重装确认(reimage-confirm)时清空。
-- reimage_prev_fp: 租期内钉死的主机指纹。买家持有 root 可读取主机私钥, 重装后新指纹不得与之相同;
--   首次完整探活通过(verified)时清空。
ALTER TABLE supplier_nodes
    ADD COLUMN reimage_required_at DATETIME NULL AFTER host_fp_claim,
    ADD COLUMN reimage_prev_fp VARCHAR(80) NULL AFTER reimage_required_at;
