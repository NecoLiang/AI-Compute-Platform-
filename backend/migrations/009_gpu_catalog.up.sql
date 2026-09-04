-- GPU/加速卡型号字典 (docs/api/gpu-catalog-api.md):
-- 平台自维护的权威型号库, 供应方发布商品时下拉选择, 替代自由文本。
-- 初始数据: 国产以 2026-05 安可认证目录为骨架(secure_certified=1), 海外取数据中心
-- 与主流消费卡。规格为公开资料初始参考值, spec_source 标注来源, 待运营按厂商官网复核。
-- fp16_tflops 口径统一为「FP16 稠密 Tensor 算力」; 规格不确定的置 NULL, 宁缺毋错。

CREATE TABLE gpu_catalog (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    vendor VARCHAR(32) NOT NULL,
    model_name VARCHAR(64) NOT NULL,
    origin ENUM('domestic','international') NOT NULL,
    grade ENUM('datacenter','consumer') NOT NULL DEFAULT 'datacenter',
    vram_gb INT NULL,
    vram_type VARCHAR(16) NULL,
    fp16_tflops DECIMAL(8,1) NULL,
    interconnect VARCHAR(32) NULL,
    secure_certified TINYINT(1) NOT NULL DEFAULT 0,
    spec_source VARCHAR(255) NULL,
    status ENUM('enabled','disabled') NOT NULL DEFAULT 'enabled',
    sort_weight INT NOT NULL DEFAULT 0,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    UNIQUE KEY uk_model (model_name),
    INDEX idx_origin_status (origin, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- ===== 海外·数据中心 =====
INSERT INTO gpu_catalog (vendor, model_name, origin, grade, vram_gb, vram_type, fp16_tflops, interconnect, secure_certified, spec_source, sort_weight) VALUES
('NVIDIA','H200-141G','international','datacenter',141,'HBM3e',989.0,'NVLink',0,'NVIDIA H200 datasheet',90),
('NVIDIA','H100-80G','international','datacenter',80,'HBM3',989.0,'NVLink',0,'NVIDIA H100 datasheet',89),
('NVIDIA','H800-80G','international','datacenter',80,'HBM3',989.0,'NVLink(带宽受限)',0,'NVIDIA H800 datasheet',88),
('NVIDIA','H20-96G','international','datacenter',96,'HBM3',148.0,'NVLink',0,'NVIDIA H20 公开规格',87),
('NVIDIA','A100-80G','international','datacenter',80,'HBM2e',312.0,'NVLink',0,'NVIDIA A100 datasheet',86),
('NVIDIA','A100-40G','international','datacenter',40,'HBM2e',312.0,'NVLink',0,'NVIDIA A100 datasheet',85),
('NVIDIA','A800-80G','international','datacenter',80,'HBM2e',312.0,'NVLink(400GB/s)',0,'NVIDIA A800 datasheet',84),
('NVIDIA','L40S-48G','international','datacenter',48,'GDDR6',181.0,'PCIe',0,'NVIDIA L40S datasheet',70),
('NVIDIA','L20-48G','international','datacenter',48,'GDDR6',119.0,'PCIe',0,'NVIDIA L20 公开规格',69),
('NVIDIA','A10-24G','international','datacenter',24,'GDDR6',125.0,'PCIe',0,'NVIDIA A10 datasheet',68),
('NVIDIA','V100-32G','international','datacenter',32,'HBM2',112.0,'NVLink',0,'NVIDIA V100 datasheet',60),
('AMD','MI300X-192G','international','datacenter',192,'HBM3',1307.0,'Infinity Fabric',0,'AMD MI300X datasheet',80);

-- ===== 海外·消费级 =====
INSERT INTO gpu_catalog (vendor, model_name, origin, grade, vram_gb, vram_type, fp16_tflops, interconnect, secure_certified, spec_source, sort_weight) VALUES
('NVIDIA','RTX 5090-32G','international','consumer',32,'GDDR7',209.0,'PCIe',0,'NVIDIA RTX 5090 公开规格',50),
('NVIDIA','RTX 4090-24G','international','consumer',24,'GDDR6X',165.0,'PCIe',0,'NVIDIA RTX 4090 datasheet',49),
('NVIDIA','RTX 3090-24G','international','consumer',24,'GDDR6X',71.0,'PCIe',0,'NVIDIA RTX 3090 datasheet',48);

-- ===== 国产·安可认证目录内 (2026-05 安全可靠测评「AI 训练与推理芯片」) =====
INSERT INTO gpu_catalog (vendor, model_name, origin, grade, vram_gb, vram_type, fp16_tflops, interconnect, secure_certified, spec_source, sort_weight) VALUES
('华为昇腾','昇腾910B','domestic','datacenter',64,'HBM2e',320.0,'HCCS',1,'安可目录+公开资料, 待官网复核',100),
('华为昇腾','昇腾310P','domestic','datacenter',24,'LPDDR4X',NULL,'PCIe',1,'安可目录; 推理卡, INT8 140TOPS',95),
('海光','K100-AI','domestic','datacenter',64,'HBM2e',NULL,'PCIe',1,'安可目录, 规格待官网复核',94),
('沐曦','曦云C500','domestic','datacenter',64,'HBM2e',NULL,'MetaXLink',1,'安可目录, 规格待官网复核',93),
('摩尔线程','MTT S4000','domestic','datacenter',48,'GDDR6',100.0,'MTLink',1,'安可目录+公开资料, 待官网复核',92),
('壁仞','BR100','domestic','datacenter',64,'HBM2e',NULL,'BLink',1,'安可目录, 规格待官网复核',91),
('天数智芯','天垓100','domestic','datacenter',32,'HBM2',147.0,'PCIe',1,'安可目录+公开资料, 待官网复核',90),
('平头哥','镇武M530','domestic','datacenter',NULL,NULL,NULL,NULL,1,'安可目录, 规格未公开',89),
('平头哥','镇武M890','domestic','datacenter',NULL,NULL,NULL,NULL,1,'安可目录, 规格未公开',88);

-- ===== 国产·目录外主流在售 =====
INSERT INTO gpu_catalog (vendor, model_name, origin, grade, vram_gb, vram_type, fp16_tflops, interconnect, secure_certified, spec_source, sort_weight) VALUES
('寒武纪','思元590','domestic','datacenter',NULL,'HBM2e',NULL,'MLU-Link',0,'公开资料口径不一, 待官网复核',85),
('寒武纪','MLU370-X8','domestic','datacenter',48,'LPDDR5',96.0,'MLU-Link',0,'寒武纪官网, 待复核',84),
('昆仑芯','P800','domestic','datacenter',96,'HBM3',NULL,'XPU-Link',0,'公开资料, 待官网复核',83),
('摩尔线程','MTT S5000','domestic','datacenter',NULL,NULL,NULL,'MTLink',0,'新品, 规格待官网复核',82),
('燧原','云燧S60','domestic','datacenter',48,NULL,NULL,'PCIe',0,'公开资料, 待官网复核',81);
