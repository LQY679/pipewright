-- 0052 pipeline_triggers:webhook_secret_ciphertext 允许为 NULL。
--
-- 背景:签名密钥经 vault(NaCl secretbox)加密存密文。此前该列 NOT NULL,
-- 要求「首个触发配置」必须能加密密钥。但若部署未配置 master key(vault 未就绪),
-- createDefault 会因无法加密而失败,连带「分支映射 / 事件 / 策略」这类与密钥无关的配置
-- 也读不出、存不进(见分支映射保存不生效的排查)。
--
-- 降级目标:master key 缺失时,允许 webhook_secret_ciphertext 为 NULL(无密钥记录),
-- 非密钥配置照常读写;密钥在 vault 就绪后经 ResetSecret 重新生成并加密。
-- 已有记录(密文非 NULL)不受影响。

PRAGMA foreign_keys = OFF;

ALTER TABLE pipeline_triggers RENAME TO pipeline_triggers_old;

CREATE TABLE IF NOT EXISTS pipeline_triggers (
    project_id                TEXT PRIMARY KEY,
    webhook_token             TEXT NOT NULL UNIQUE,
    webhook_secret_ciphertext BLOB,
    events_json               TEXT NOT NULL DEFAULT '{}',
    branch_mappings_json      TEXT NOT NULL DEFAULT '[]',
    path_filters_json         TEXT NOT NULL DEFAULT '[]',
    unmatched_policy          TEXT NOT NULL DEFAULT 'record',
    created_at                TEXT NOT NULL,
    updated_at                TEXT NOT NULL,
    FOREIGN KEY (project_id) REFERENCES projects (id) ON DELETE CASCADE
);

INSERT INTO pipeline_triggers (
    project_id, webhook_token, webhook_secret_ciphertext, events_json,
    branch_mappings_json, path_filters_json, unmatched_policy, created_at, updated_at
)
SELECT
    project_id, webhook_token, webhook_secret_ciphertext, events_json,
    branch_mappings_json, path_filters_json, unmatched_policy, created_at, updated_at
FROM pipeline_triggers_old;

DROP TABLE pipeline_triggers_old;

PRAGMA foreign_keys = ON;
