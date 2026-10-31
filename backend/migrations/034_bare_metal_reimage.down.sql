-- supplier_nodes.reimage_required_at / reimage_prev_fp 是整机重装闸门(docs/25 §25.7.4): 删除后买家曾持有 root 的节点
-- 不经重装确认即可回到可售路径。027 之后的迁移统一拒绝自动回退(031 纯探活遥测除外), 回滚需先由运营逐台结案。
SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = '034 downgrade would remove the bare-metal reimage gate; automatic column removal is disabled';
