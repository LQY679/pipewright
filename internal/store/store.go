// Package store owns all SQLite access. No other package touches the database
// directly — they go through repository interfaces defined here.
//
// 使用纯 Go 的 modernc.org/sqlite 驱动(无 CGO),以支持 CGO_DISABLED 全静态
// 交叉编译——这是平台"双运行模式"的前提。严禁改用需要 CGO 的 mattn/go-sqlite3。
package store

import (
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	_ "modernc.org/sqlite" // pure-Go SQLite driver (no CGO)
)

//go:embed migrations/sqlite/*.sql
var sqliteMigrationFS embed.FS

//go:embed migrations/mysql/*.sql
var mysqlMigrationFS embed.FS

// Store 持有数据库连接。仅本包持有 *sql.DB;其它领域包经 repository 接口取数。
type Store struct {
	DB *sql.DB
	// Dialect 标识底层方言(SQLite 默认 / MySQL);Open 时由驱动判定。
	Dialect Dialect
}

// OpenConfig 是驱动感知的打开参数。Driver 取 "sqlite"(默认)或 "mysql";
// DSN 为对应驱动的连接串(SQLite 为数据库文件路径)。
type OpenConfig struct {
	Driver string
	DSN    string
}

// OpenWithConfig 按驱动选择后端打开数据库并应用对应方言的内嵌迁移。
// 空 Driver 视为 "sqlite",保持向后兼容。
func OpenWithConfig(c OpenConfig) (*Store, error) {
	switch c.Driver {
	case "mysql":
		return openMySQL(c.DSN)
	case "", "sqlite":
		return Open(c.DSN)
	default:
		return nil, fmt.Errorf("store: unknown db driver %q (want sqlite|mysql)", c.Driver)
	}
}

// Open 打开(必要时创建)指定路径的 SQLite 数据库,并应用内嵌迁移。
// DSN 加固:busy_timeout 避免锁竞争直接报错;WAL 提升并发;foreign_keys 默认开启
// (SQLite 默认关闭)。SetMaxOpenConns(1) 串行化写,规避 SQLITE_BUSY。
func Open(dbPath string) (*Store, error) {
	dsn := dbPath + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(wal)&_pragma=foreign_keys(on)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	s := &Store{DB: db, Dialect: DialectOf(db)}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// openMySQL 打开 MySQL(8.0+)连接并应用 mysql 方言迁移。与 SQLite 不同,
// MySQL 用常规连接池(不串行化),外键由 InnoDB 默认强制。驱动经 dialect.go
// 的 mysql 包导入已注册,无需在此重复 blank import。
//
// ClientFoundRows 强制开启:让 UPDATE 的 RowsAffected 返回"匹配行数"而非默认的
// "实际改变行数",与 SQLite 语义一致。否则"把列更新为相同值"会返回 0,被领域层
// (如 SetDeployTerminal / 各 RowsAffected==0→ErrNotFound 校验)误判为行不存在。
func openMySQL(dsn string) (*Store, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse mysql dsn: %w", err)
	}
	cfg.ClientFoundRows = true
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return nil, fmt.Errorf("open mysql: %w", err)
	}
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(25)
	db.SetConnMaxLifetime(5 * time.Minute)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping mysql: %w", err)
	}
	s := &Store{DB: db, Dialect: DialectOf(db)}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// migrationFS 返回当前方言对应的内嵌迁移文件系统与 glob。
func (s *Store) migrationFS() (fs.FS, string) {
	if s.Dialect == MySQL {
		return mysqlMigrationFS, "migrations/mysql/*.sql"
	}
	return sqliteMigrationFS, "migrations/sqlite/*.sql"
}

// Close 关闭底层数据库连接。
func (s *Store) Close() error { return s.DB.Close() }

// migrate 建立 schema_migrations 跟踪表,并按版本顺序幂等应用内嵌的 *.sql 迁移。
// 领域表由各自 story 在需要时通过新增迁移创建。bootstrap DDL 与每条迁移的执行
// 方式按方言分叉(见 schemaMigrationsDDL / applyMigration)。
func (s *Store) migrate() error {
	if _, err := s.DB.Exec(s.schemaMigrationsDDL()); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	fsys, glob := s.migrationFS()
	entries, err := fs.Glob(fsys, glob)
	if err != nil {
		return fmt.Errorf("glob migrations: %w", err)
	}
	sort.Strings(entries)

	for _, entry := range entries {
		version := migrationVersion(entry)

		var applied int
		if err := s.DB.QueryRow(
			`SELECT COUNT(1) FROM schema_migrations WHERE version = ?`, version,
		).Scan(&applied); err != nil {
			return fmt.Errorf("check migration %s: %w", version, err)
		}
		if applied > 0 {
			continue
		}

		sqlText, err := fs.ReadFile(fsys, entry)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", version, err)
		}
		if err := s.applyMigration(version, string(sqlText)); err != nil {
			return err
		}
	}
	return nil
}

// schemaMigrationsDDL 返回方言对应的版本跟踪表 DDL。MySQL 主键不能用 TEXT,改 VARCHAR。
func (s *Store) schemaMigrationsDDL() string {
	if s.Dialect == MySQL {
		return `CREATE TABLE IF NOT EXISTS schema_migrations (
			version    VARCHAR(255) PRIMARY KEY,
			applied_at VARCHAR(64) NOT NULL
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`
	}
	return `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    TEXT PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`
}

// applyMigration 应用单条迁移并记录版本。
//
// SQLite:DDL 与版本记录在同一事务内原子提交,崩溃/部分失败整体回滚,不留半迁移。
// MySQL :DDL 隐式提交,事务对 DDL 无回滚效力——故逐句执行(go-sql-driver 默认不允许
// 单 Exec 多语句)后再记录版本。CREATE TABLE/TRIGGER 靠 IF NOT EXISTS 幂等;
// ALTER ADD COLUMN 无 IF NOT EXISTS,故对 ADD COLUMN 在跑前查 INFORMATION_SCHEMA
// 守卫(列已存在则跳过),避免重跑撞 "Duplicate column name" 直接退出、引发重启循环
// (见迁移 0049 在 DDL 已提交/版本未记录间崩溃的案例)。
func (s *Store) applyMigration(version, sqlText string) error {
	now := time.Now().UTC().Format(time.RFC3339)

	if s.Dialect == MySQL {
		for _, stmt := range splitStatements(sqlText) {
			// MySQL 的 ALTER ... ADD COLUMN 无 IF NOT EXISTS。DDL 隐式提交,
			// 若进程恰好在 DDL 已提交、版本未记录之间崩溃,重启重跑会撞
			// "Duplicate column name" 并直接退出(见 0049 案例,导致 systemd 重启循环)。
			// 故对 ADD COLUMN 做幂等守卫:列已存在则跳过,不让重跑致命。
			// MySQL 的 ALTER ADD COLUMN / CREATE INDEX 均无 IF NOT EXISTS。DDL 隐式提交,
			// 若进程恰好在 DDL 已提交、版本未记录之间崩溃,重启重跑会撞 "Duplicate column
			// name" / "Duplicate key name" 并直接退出(见 0049 案例,导致 systemd 重启循环)。
			// 故对这两类语句做幂等守卫:对象已存在则跳过,不让重跑致命。
			if table, col, ok := parseAddColumn(stmt); ok {
				exists, err := columnExists(s.DB, table, col)
				if err != nil {
					return fmt.Errorf("apply migration %s: %w", version, err)
				}
				if exists {
					continue
				}
			}
			if table, idx, ok := parseCreateIndex(stmt); ok {
				exists, err := indexExists(s.DB, table, idx)
				if err != nil {
					return fmt.Errorf("apply migration %s: %w", version, err)
				}
				if exists {
					continue
				}
			}
			if _, err := s.DB.Exec(stmt); err != nil {
				return fmt.Errorf("apply migration %s: %w", version, err)
			}
		}
		if _, err := s.DB.Exec(
			`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, version, now,
		); err != nil {
			return fmt.Errorf("record migration %s: %w", version, err)
		}
		return nil
	}

	tx, err := s.DB.Begin()
	if err != nil {
		return fmt.Errorf("begin migration %s: %w", version, err)
	}
	if _, err := tx.Exec(sqlText); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("apply migration %s: %w", version, err)
	}
	if _, err := tx.Exec(
		`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, version, now,
	); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("record migration %s: %w", version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %s: %w", version, err)
	}
	return nil
}

// parseAddColumn 从 "ALTER TABLE <t> ADD COLUMN <c> ..." 解析表名与列名
// (大小写、额外空格、反引号均容错)。匹配返回 ok=true,供 applyMigration 做幂等守卫。
func parseAddColumn(stmt string) (table, column string, ok bool) {
	s := strings.TrimSpace(stmt)
	re := `(?i)^ALTER\s+TABLE\s+` + "`?" + `(\w+)` + "`?" + `\s+ADD\s+COLUMN\s+` + "`?" + `(\w+)` + "`?"
	m := regexp.MustCompile(re).FindStringSubmatch(s)
	if len(m) < 3 {
		return "", "", false
	}
	return m[1], m[2], true
}

// columnExists 查询 INFORMATION_SCHEMA 判断 MySQL 中某表某列是否已存在。
func columnExists(db *sql.DB, table, column string) (bool, error) {
	var cnt int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS
		 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?`,
		table, column,
	).Scan(&cnt)
	if err != nil {
		return false, err
	}
	return cnt > 0, nil
}

// parseCreateIndex 从 "CREATE [UNIQUE] INDEX <idx> ON <t> (...)" 解析索引名与表名
// (大小写、反引号均容错)。匹配返回 ok=true,供 applyMigration 做幂等守卫。
func parseCreateIndex(stmt string) (table, index string, ok bool) {
	s := strings.TrimSpace(stmt)
	re := `(?i)^CREATE\s+(UNIQUE\s+)?INDEX\s+` + "`?" + `(\w+)` + "`?" + `\s+ON\s+` + "`?" + `(\w+)` + "`?"
	m := regexp.MustCompile(re).FindStringSubmatch(s)
	if len(m) < 4 {
		return "", "", false
	}
	return m[3], m[2], true
}

// indexExists 查询 INFORMATION_SCHEMA.STATISTICS 判断 MySQL 中某表某索引是否已存在。
func indexExists(db *sql.DB, table, index string) (bool, error) {
	var cnt int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM INFORMATION_SCHEMA.STATISTICS
		 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND INDEX_NAME = ?`,
		table, index,
	).Scan(&cnt)
	if err != nil {
		return false, err
	}
	return cnt > 0, nil
}

// splitStatements 把含多条 SQL 的迁移文本拆成单条语句(供 MySQL 逐条执行)。
// 规则:剥离整行/行尾 `--` 注释;在单引号字符串外按 `;` 切分;丢弃空白语句。
// 我们的 mysql 迁移不在字符串字面量内出现 `;`,且触发器写成单语句 SIGNAL 形式
// (无 BEGIN/END、体内无 `;`),故此简单切分足够且可单测。
func splitStatements(sqlText string) []string {
	var stmts []string
	var b strings.Builder
	inQuote := false
	lineComment := false

	runes := []rune(sqlText)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		if lineComment {
			if c == '\n' {
				lineComment = false
				b.WriteRune(c)
			}
			continue
		}
		if !inQuote && c == '-' && i+1 < len(runes) && runes[i+1] == '-' {
			lineComment = true
			i++ // 跳过第二个 '-'
			continue
		}
		if c == '\'' {
			inQuote = !inQuote
			b.WriteRune(c)
			continue
		}
		if c == ';' && !inQuote {
			if stmt := strings.TrimSpace(b.String()); stmt != "" {
				stmts = append(stmts, stmt)
			}
			b.Reset()
			continue
		}
		b.WriteRune(c)
	}
	if stmt := strings.TrimSpace(b.String()); stmt != "" {
		stmts = append(stmts, stmt)
	}
	return stmts
}

// migrationVersion 从 "migrations/sqlite/0001_baseline.sql" 提取版本键 "0001_baseline"。
func migrationVersion(entry string) string {
	return strings.TrimSuffix(path.Base(entry), ".sql")
}
