package store

import (
	"errors"
	"fmt"
	"strings"
)

// copyTables are copied in this order; ids are kept, so request logs still
// point at their keys.
var copyTables = []string{"providers", "models", "api_keys", "request_logs"}

// CopyTo copies everything (configuration, admin token, request logs) into
// dst, which must hold no providers, models, keys or logs. Used to move
// between SQLite and PostgreSQL; progress is called after each batch.
func (s *Store) CopyTo(dst *Store, progress func(table string, copied int64)) error {
	s.FlushLogs()
	dst.FlushLogs()
	for _, t := range copyTables {
		var n int64
		if err := dst.db.QueryRow(`SELECT COUNT(*) FROM ` + t).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return fmt.Errorf("target database is not empty (%s has %d rows)", t, n)
		}
	}
	dst.mu.Lock()
	defer dst.mu.Unlock()

	// settings: key/value rows, including the generated admin token
	rows, err := s.db.Query(`SELECT k, v FROM settings`)
	if err != nil {
		return err
	}
	var kv [][2]string
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			rows.Close()
			return err
		}
		kv = append(kv, [2]string{k, v})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, p := range kv {
		if _, err := dst.db.Exec(`INSERT INTO settings (k, v) VALUES (?, ?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, p[0], p[1]); err != nil {
			return err
		}
	}
	if progress != nil {
		progress("settings", int64(len(kv)))
	}

	for _, t := range copyTables {
		if err := s.copyTable(dst, t, progress); err != nil {
			return fmt.Errorf("%s: %w", t, err)
		}
	}
	// the hourly rollup is derived: rebuild it from the copied logs
	if err := dst.RebuildStats(); err != nil {
		return fmt.Errorf("request_stats: %w", err)
	}
	return dst.reloadLocked()
}

func (s *Store) copyTable(dst *Store, table string, progress func(string, int64)) error {
	// the target's columns: both databases are migrated to the same version,
	// but older SQLite files list later-added columns in a different order
	probe, err := dst.db.Query(`SELECT * FROM ` + table + ` WHERE 1=0`)
	if err != nil {
		return err
	}
	cols, err := probe.Columns()
	probe.Close()
	if err != nil {
		return err
	}
	if len(cols) == 0 || cols[0] != "id" {
		return errors.New("unexpected table layout")
	}
	colList := strings.Join(cols, ", ")
	const page, chunk = 5000, 500 // chunk * columns stays under both databases' parameter limits
	row := "(" + placeholders(len(cols)) + ")"
	var lastID, copied int64
	for {
		rows, err := s.db.Query(`SELECT `+colList+` FROM `+table+` WHERE id > ? ORDER BY id LIMIT ?`, lastID, page)
		if err != nil {
			return err
		}
		var batch [][]any
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, vals)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(batch) == 0 {
			break
		}
		tx, err := dst.db.Begin()
		if err != nil {
			return err
		}
		for i := 0; i < len(batch); i += chunk {
			part := batch[i:min(i+chunk, len(batch))]
			args := make([]any, 0, len(part)*len(cols))
			for _, vals := range part {
				args = append(args, vals...)
			}
			values := strings.TrimSuffix(strings.Repeat(row+",", len(part)), ",")
			if _, err := tx.Exec(`INSERT INTO `+table+` (`+colList+`) VALUES `+values, args...); err != nil {
				tx.Rollback()
				return err
			}
		}
		if err := tx.resetID(table); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		lastID = toInt64(batch[len(batch)-1][0])
		copied += int64(len(batch))
		if progress != nil {
			progress(table, copied)
		}
		if len(batch) < page {
			break
		}
	}
	if copied == 0 && progress != nil {
		progress(table, 0)
	}
	return nil
}

func toInt64(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int32:
		return int64(x)
	case int:
		return int64(x)
	case float64:
		return int64(x)
	}
	return 0
}
