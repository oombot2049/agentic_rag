package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
)

const migrationLockID int64 = 0x4167656e746963

var migrationName = regexp.MustCompile(`^(\d{6})_([a-z][a-z0-9_]*)\.(up|down)\.sql$`)

type migration struct {
	Version  int64
	Name     string
	UpSQL    string
	DownSQL  string
	Checksum [sha256.Size]byte
}

type Migrator struct {
	pool *Pool
	fs   fs.FS
	dir  string
}

func NewMigrator(pool *Pool, migrations fs.FS, directory string) (*Migrator, error) {
	if pool == nil || migrations == nil {
		return nil, errors.New("postgres migrator requires pool and filesystem")
	}
	if _, err := loadMigrations(migrations, directory); err != nil {
		return nil, err
	}
	return &Migrator{pool: pool, fs: migrations, dir: directory}, nil
}

func (m *Migrator) Up(ctx context.Context) (err error) {
	migrations, err := loadMigrations(m.fs, m.dir)
	if err != nil {
		return err
	}
	return m.withLock(ctx, func(conn *sql.Conn) error {
		if err := ensureLedger(ctx, conn); err != nil {
			return err
		}
		applied, err := appliedMigrations(ctx, conn)
		if err != nil {
			return err
		}
		for _, item := range migrations {
			if checksum, ok := applied[item.Version]; ok {
				if checksum != item.Checksum {
					return fmt.Errorf("migration %06d checksum drift", item.Version)
				}
				continue
			}
			if err := applyUp(ctx, conn, item); err != nil {
				return err
			}
		}
		return nil
	})
}

func (m *Migrator) Down(ctx context.Context, steps int) error {
	if steps < 1 {
		return errors.New("migration down steps must be positive")
	}
	migrations, err := loadMigrations(m.fs, m.dir)
	if err != nil {
		return err
	}
	byVersion := make(map[int64]migration, len(migrations))
	for _, item := range migrations {
		byVersion[item.Version] = item
	}
	return m.withLock(ctx, func(conn *sql.Conn) error {
		if err := ensureLedger(ctx, conn); err != nil {
			return err
		}
		rows, err := conn.QueryContext(ctx, `SELECT version FROM schema_migrations ORDER BY version DESC LIMIT $1`, steps)
		if err != nil {
			return err
		}
		var versions []int64
		for rows.Next() {
			var version int64
			if err := rows.Scan(&version); err != nil {
				rows.Close()
				return err
			}
			versions = append(versions, version)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, version := range versions {
			item, ok := byVersion[version]
			if !ok || item.DownSQL == "" {
				return fmt.Errorf("migration %06d has no down migration", version)
			}
			if err := applyDown(ctx, conn, item); err != nil {
				return err
			}
		}
		return nil
	})
}

func (m *Migrator) withLock(ctx context.Context, work func(*sql.Conn) error) (err error) {
	conn, err := m.pool.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, migrationLockID); err != nil {
		return err
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), m.pool.healthTimeout)
		defer cancel()
		_, unlockErr := conn.ExecContext(unlockCtx, `SELECT pg_advisory_unlock($1)`, migrationLockID)
		err = errors.Join(err, unlockErr)
	}()
	return work(conn)
}

func ensureLedger(ctx context.Context, conn *sql.Conn) error {
	_, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version bigint PRIMARY KEY, name text NOT NULL, checksum bytea NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`)
	return err
}

func appliedMigrations(ctx context.Context, conn *sql.Conn) (map[int64][sha256.Size]byte, error) {
	rows, err := conn.QueryContext(ctx, `SELECT version, checksum FROM schema_migrations ORDER BY version`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[int64][sha256.Size]byte)
	for rows.Next() {
		var version int64
		var raw []byte
		if err := rows.Scan(&version, &raw); err != nil {
			return nil, err
		}
		if len(raw) != sha256.Size {
			return nil, fmt.Errorf("migration %06d has invalid checksum", version)
		}
		var sum [sha256.Size]byte
		copy(sum[:], raw)
		result[version] = sum
	}
	return result, rows.Err()
}

func applyUp(ctx context.Context, conn *sql.Conn, item migration) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, item.UpSQL); err == nil {
		_, err = tx.ExecContext(ctx, `INSERT INTO schema_migrations(version,name,checksum) VALUES($1,$2,$3)`, item.Version, item.Name, item.Checksum[:])
	}
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("apply migration %06d up: %w", item.Version, err)
	}
	return tx.Commit()
}

func applyDown(ctx context.Context, conn *sql.Conn, item migration) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, item.DownSQL); err == nil {
		_, err = tx.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version=$1`, item.Version)
	}
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("apply migration %06d down: %w", item.Version, err)
	}
	return tx.Commit()
}

func loadMigrations(fsys fs.FS, directory string) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, directory)
	if err != nil {
		return nil, err
	}
	items := map[int64]*migration{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		match := migrationName.FindStringSubmatch(entry.Name())
		if match == nil {
			return nil, fmt.Errorf("invalid migration filename %q", entry.Name())
		}
		version, _ := strconv.ParseInt(match[1], 10, 64)
		item := items[version]
		if item == nil {
			item = &migration{Version: version, Name: match[2]}
			items[version] = item
		} else if item.Name != match[2] {
			return nil, fmt.Errorf("migration version %06d has conflicting names", version)
		}
		data, err := fs.ReadFile(fsys, path.Join(directory, entry.Name()))
		if err != nil {
			return nil, err
		}
		if match[3] == "up" {
			if item.UpSQL != "" {
				return nil, fmt.Errorf("duplicate migration %06d up", version)
			}
			item.UpSQL = string(data)
			item.Checksum = sha256.Sum256(data)
		} else {
			if item.DownSQL != "" {
				return nil, fmt.Errorf("duplicate migration %06d down", version)
			}
			item.DownSQL = string(data)
		}
	}
	versions := make([]int64, 0, len(items))
	for version := range items {
		versions = append(versions, version)
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i] < versions[j] })
	result := make([]migration, 0, len(versions))
	for i, version := range versions {
		item := items[version]
		if item.UpSQL == "" || item.DownSQL == "" {
			return nil, fmt.Errorf("migration %06d requires up and down files", version)
		}
		if i > 0 && version != versions[i-1]+1 {
			return nil, fmt.Errorf("migration gap between %06d and %06d", versions[i-1], version)
		}
		result = append(result, *item)
	}
	return result, nil
}
