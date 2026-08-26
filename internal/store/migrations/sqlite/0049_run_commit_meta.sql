-- 0049: 提交元数据持久化。
-- 把克隆解析出的实际检出提交「作者/备注/时间」落到 pipeline_runs,
-- 供「通知」设置里的全局通知模板占位 {{commitAuthor}}/{{commitMessage}}/{{commitTime}} 取用
-- (画布 notify 节点内联模板早已支持;此处补齐 run 终态全局通知路径)。
-- 空串 = 未解析到(老运行 / 非 git 源),对应模板占位渲染为空串。
ALTER TABLE pipeline_runs ADD COLUMN trigger_commit_author TEXT NOT NULL DEFAULT '';
ALTER TABLE pipeline_runs ADD COLUMN trigger_commit_message TEXT NOT NULL DEFAULT '';
ALTER TABLE pipeline_runs ADD COLUMN trigger_commit_time TEXT NOT NULL DEFAULT '';
