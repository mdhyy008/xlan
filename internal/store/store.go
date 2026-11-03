package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"

	"xlan/core"
)

const schema = `
CREATE TABLE IF NOT EXISTS items (
  id         TEXT PRIMARY KEY,
  kind       TEXT NOT NULL,
  from_id    TEXT NOT NULL,
  from_name  TEXT,
  to_id      TEXT NOT NULL,
  channel    TEXT NOT NULL,
  body       TEXT,
  name       TEXT,
  size       INTEGER,
  sha256     TEXT,
  status     TEXT NOT NULL,
  path       TEXT,
  manifest   TEXT,
  ranges     TEXT,
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_items_created ON items(created_at DESC);

CREATE TABLE IF NOT EXISTS peers (
  id        TEXT PRIMARY KEY,
  name      TEXT NOT NULL,
  addr      TEXT,
  port      INTEGER,
  platform  TEXT,
  last_seen INTEGER NOT NULL,
  trusted   INTEGER NOT NULL DEFAULT 0
);
`

// Store 封装 SQLite。
type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	// 旧库补列，失败说明列已存在。
	_, _ = db.Exec(`ALTER TABLE items ADD COLUMN manifest TEXT`)
	_, _ = db.Exec(`ALTER TABLE items ADD COLUMN ranges TEXT`)
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

const itemCols = `id, kind, from_id, from_name, to_id, channel, body, name, size, sha256, status, path, created_at`

func scanItem(row interface{ Scan(...any) error }) (core.Item, error) {
	var it core.Item
	var kind, channel, status string
	// 这些列历史上可能是 NULL，一律用 NullString 兜住，否则一行脏数据会让整个列表读不出来。
	var fromID, fromName, toID, body, name, sha, path sql.NullString
	var size sql.NullInt64
	err := row.Scan(&it.ID, &kind, &fromID, &fromName, &toID, &channel,
		&body, &name, &size, &sha, &status, &path, &it.CreatedAt)
	if err != nil {
		return core.Item{}, err
	}
	it.FromID = fromID.String
	it.FromName = fromName.String
	it.ToID = toID.String
	it.Kind = core.Kind(kind)
	it.Channel = core.Channel(channel)
	it.Status = core.Status(status)
	it.Body = body.String
	it.Name = name.String
	it.SHA256 = sha.String
	it.Path = path.String
	it.Size = size.Int64
	return it, nil
}

func (s *Store) AddItem(it core.Item) error {
	_, err := s.db.Exec(`INSERT INTO items(`+itemCols+`)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
		  status=excluded.status, path=excluded.path, sha256=excluded.sha256,
		  size=excluded.size, body=excluded.body`,
		it.ID, string(it.Kind), it.FromID, it.FromName, it.ToID, string(it.Channel),
		it.Body, it.Name, it.Size, it.SHA256, string(it.Status), it.Path, it.CreatedAt)
	return err
}

func (s *Store) GetItem(id string) (core.Item, bool, error) {
	row := s.db.QueryRow(`SELECT `+itemCols+` FROM items WHERE id = ?`, id)
	it, err := scanItem(row)
	if err == sql.ErrNoRows {
		return core.Item{}, false, nil
	}
	if err != nil {
		return core.Item{}, false, err
	}
	return it, true, nil
}

// ListItems 按时间倒序返回，limit<=0 表示不限。
func (s *Store) ListItems(limit int, since int64) ([]core.Item, error) {
	q := `SELECT ` + itemCols + ` FROM items`
	args := []any{}
	if since > 0 {
		q += ` WHERE created_at >= ?`
		args = append(args, since)
	}
	q += ` ORDER BY created_at DESC`
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []core.Item{}
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// ListActive 返回所有处于 active 的条目（用于清理卡死的传输）。
func (s *Store) ListActive() ([]core.Item, error) {
	rows, err := s.db.Query(`SELECT `+itemCols+` FROM items WHERE status = ?`, string(core.StatusActive))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []core.Item{}
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

func (s *Store) UpdateItemStatus(id string, st core.Status, path string) error {
	_, err := s.db.Exec(`UPDATE items SET status = ?, path = ? WHERE id = ?`, string(st), path, id)
	return err
}

func (s *Store) SetManifest(id, manifest string) error {
	_, err := s.db.Exec(`UPDATE items SET manifest = ? WHERE id = ?`, manifest, id)
	return err
}

func (s *Store) GetManifest(id string) (string, error) {
	var m sql.NullString
	err := s.db.QueryRow(`SELECT manifest FROM items WHERE id = ?`, id).Scan(&m)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return m.String, err
}

func (s *Store) SetRanges(id, data string) error {
	_, err := s.db.Exec(`UPDATE items SET ranges = ? WHERE id = ?`, data, id)
	return err
}

func (s *Store) GetRanges(id string) (string, error) {
	var v sql.NullString
	err := s.db.QueryRow(`SELECT ranges FROM items WHERE id = ?`, id).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v.String, err
}

func (s *Store) DeleteItem(id string) error {
	_, err := s.db.Exec(`DELETE FROM items WHERE id = ?`, id)
	return err
}

// UpsertPeer 更新设备信息，但不覆盖已有的 trusted 标记。
func (s *Store) UpsertPeer(p core.Peer) error {
	_, err := s.db.Exec(`INSERT INTO peers(id, name, addr, port, platform, last_seen, trusted)
		VALUES(?,?,?,?,?,?,0)
		ON CONFLICT(id) DO UPDATE SET
		  name=excluded.name, addr=excluded.addr, port=excluded.port,
		  platform=excluded.platform, last_seen=excluded.last_seen`,
		p.ID, p.Name, p.Addr, p.Port, p.Platform, p.LastSeen)
	return err
}

func (s *Store) ListPeers() ([]core.Peer, error) {
	rows, err := s.db.Query(`SELECT id, name, addr, port, platform, last_seen, trusted
		FROM peers ORDER BY last_seen DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []core.Peer{}
	for rows.Next() {
		var p core.Peer
		var trusted int
		var addr, platform sql.NullString
		var port sql.NullInt64
		if err := rows.Scan(&p.ID, &p.Name, &addr, &port, &platform, &p.LastSeen, &trusted); err != nil {
			return nil, err
		}
		p.Addr = addr.String
		p.Platform = platform.String
		p.Port = int(port.Int64)
		p.Trusted = trusted == 1
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) GetPeer(id string) (core.Peer, bool, error) {
	peers, err := s.ListPeers()
	if err != nil {
		return core.Peer{}, false, err
	}
	for _, p := range peers {
		if p.ID == id {
			return p, true, nil
		}
	}
	return core.Peer{}, false, nil
}

func (s *Store) SetTrusted(id string, v bool) error {
	n := 0
	if v {
		n = 1
	}
	_, err := s.db.Exec(`UPDATE peers SET trusted = ? WHERE id = ?`, n, id)
	return err
}

func (s *Store) DeletePeer(id string) error {
	_, err := s.db.Exec(`DELETE FROM peers WHERE id = ?`, id)
	return err
}
