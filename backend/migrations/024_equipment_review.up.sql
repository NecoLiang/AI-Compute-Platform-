-- 设备商品审核留痕: 驳回原因落库(与 products.rejected_reason / 015 同口径),
-- 供应方在「修改并重提」时可看到不符合项; 重提后清空并回到 pending 重新审核。
ALTER TABLE equipment_products ADD COLUMN rejected_reason VARCHAR(256) NOT NULL DEFAULT '' AFTER status;
