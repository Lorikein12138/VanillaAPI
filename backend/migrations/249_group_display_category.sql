-- 分组展示分类（二开）：模型广场与创建密钥的分组归类。
-- 空字符串表示按平台自动归类，已有分组保持原归类。
ALTER TABLE groups
    ADD COLUMN IF NOT EXISTS display_category VARCHAR(20) NOT NULL DEFAULT '';
