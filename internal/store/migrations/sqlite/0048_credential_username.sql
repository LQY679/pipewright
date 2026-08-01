-- Optional Git HTTPS username. Gitee organization repositories require the
-- authenticated account name rather than the repository owner.
ALTER TABLE credentials ADD COLUMN username TEXT NOT NULL DEFAULT '';
