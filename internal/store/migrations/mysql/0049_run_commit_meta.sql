-- 0049: 提交元数据持久化(MySQL 方言;语义与 sqlite/0049 相同)。
ALTER TABLE pipeline_runs ADD COLUMN trigger_commit_author VARCHAR(255) NOT NULL DEFAULT '';
ALTER TABLE pipeline_runs ADD COLUMN trigger_commit_message VARCHAR(1000) NOT NULL DEFAULT '';
ALTER TABLE pipeline_runs ADD COLUMN trigger_commit_time VARCHAR(64) NOT NULL DEFAULT '';
