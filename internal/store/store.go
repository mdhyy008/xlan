package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

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
  duplicate  INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_items_created ON items(created_at DESC);

CREATE TABLE IF NOT EXISTS history (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  ts          INTEGER NOT NULL,
  direction   TEXT NOT NULL,
  peer_name   TEXT,
  name        TEXT,
  kind        TEXT,
  size        INTEGER,
  status      TEXT NOT NULL,
  duration_ms INTEGER
);
CREATE INDEX IF NOT EXISTS idx_history_ts ON history(ts DESC);

CREATE TABLE IF NOT EXISTS peers (
  id        TEXT PRIMARY KEY,
  name      TEXT NOT NULL,
  addr      TEXT,
  port      INTEGER,
  platform  TEXT,
  last_seen INTEGER NOT NULL,
  trusted   INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
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
	_, _ = db.Exec(`ALTER TABLE items ADD COLUMN duplicate INTEGER NOT NULL DEFAULT 0`)
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

const itemCols = `id, kind, from_id, from_name, to_id, channel, body, name, size, sha256, status, path, created_at, duplicate`

func scanItem(row interface{ Scan(...any) error }) (core.Item, error) {
	var it core.Item
	var kind, channel, status string
	// 这些列历史上可能是 NULL，一律用 NullString 兜住，否则一行脏数据会让整个列表读不出来。
	var fromID, fromName, toID, body, name, sha, path sql.NullString
	var size sql.NullInt64
	var duplicate int
	err := row.Scan(&it.ID, &kind, &fromID, &fromName, &toID, &channel,
		&body, &name, &size, &sha, &status, &path, &it.CreatedAt, &duplicate)
	if err != nil {
		return core.Item{}, err
	}
	it.Duplicate = duplicate != 0
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
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
		  status=excluded.status, path=excluded.path, sha256=excluded.sha256,
		  size=excluded.size, body=excluded.body, duplicate=excluded.duplicate`,
		it.ID, string(it.Kind), it.FromID, it.FromName, it.ToID, string(it.Channel),
		it.Body, it.Name, it.Size, it.SHA256, string(it.Status), it.Path, it.CreatedAt, it.Duplicate)
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

// ListItems 按时间倒序返回，limit<=0 表示不限。offset 用于分页，
// 排序带上 id，避免同一秒内的记录在不同页之间错位。
func (s *Store) ListItems(limit int, since int64, offset int) ([]core.Item, error) {
	q := `SELECT ` + itemCols + ` FROM items`
	args := []any{}
	if since > 0 {
		q += ` WHERE created_at >= ?`
		args = append(args, since)
	}
	q += ` ORDER BY created_at DESC, id DESC`
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
		if offset > 0 {
			q += ` OFFSET ?`
			args = append(args, offset)
		}
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

// ListPublic 按时间倒序返回公共频道的文本消息，用于和其他设备对齐历史。
func (s *Store) ListPublic(limit int) ([]core.Item, error) {
	q := `SELECT ` + itemCols + ` FROM items WHERE channel = ? AND kind = ? ORDER BY created_at DESC`
	args := []any{string(core.ChanPublic), string(core.KindText)}
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

// CountPublic 返回本机保存的公共频道文本消息条数，用于挑记录最全的设备。
func (s *Store) CountPublic() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM items WHERE channel = ? AND kind = ?`,
		string(core.ChanPublic), string(core.KindText)).Scan(&n)
	return n, err
}

// ClearPublic 删除 cutoff 及以前的公共频道文本消息。
func (s *Store) ClearPublic(cutoff int64) (int, error) {
	res, err := s.db.Exec(`DELETE FROM items WHERE channel = ? AND kind = ? AND created_at <= ?`,
		string(core.ChanPublic), string(core.KindText), cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// MetaInt 读取整数型元数据，键不存在时返回 0。
func (s *Store) MetaInt(key string) (int64, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, nil
	}
	return n, nil
}

// SetMetaInt 写入整数型元数据。
func (s *Store) SetMetaInt(key string, v int64) error {
	_, err := s.db.Exec(`INSERT INTO meta(key, value) VALUES(?, ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, strconv.FormatInt(v, 10))
	return err
}

// ListPending 返回所有还没被处理的 offer（用于清理过期的待接收条目）。
func (s *Store) ListPending() ([]core.Item, error) {
	rows, err := s.db.Query(`SELECT `+itemCols+` FROM items WHERE status = ?`, string(core.StatusPending))
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

// ListBefore 返回早于 cutoff 的条目。
func (s *Store) ListBefore(cutoff int64, limit int) ([]core.Item, error) {
	q := `SELECT ` + itemCols + ` FROM items WHERE created_at < ? ORDER BY created_at ASC`
	args := []any{cutoff}
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

// DeleteBefore 删掉早于 cutoff 的条目，返回删除的行数。
func (s *Store) DeleteBefore(cutoff int64) (int, error) {
	res, err := s.db.Exec(`DELETE FROM items WHERE created_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (s *Store) AddHistory(r core.TransferRecord) error {
	_, err := s.db.Exec(`INSERT INTO history(ts,direction,peer_name,name,kind,size,status,duration_ms)
		VALUES(?,?,?,?,?,?,?,?)`,
		r.Ts, r.Direction, r.PeerName, r.Name, r.Kind, r.Size, r.Status, r.DurationMS)
	return err
}

func (s *Store) ListHistory(limit int) ([]core.TransferRecord, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT id,ts,direction,peer_name,name,kind,size,status,duration_ms
		FROM history ORDER BY ts DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []core.TransferRecord{}
	for rows.Next() {
		var r core.TransferRecord
		var peer, name, kind sql.NullString
		var size, dur sql.NullInt64
		if err := rows.Scan(&r.ID, &r.Ts, &r.Direction, &peer, &name, &kind, &size, &r.Status, &dur); err != nil {
			return nil, err
		}
		r.PeerName, r.Name, r.Kind = peer.String, name.String, kind.String
		r.Size, r.DurationMS = size.Int64, dur.Int64
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) DeleteHistoryBefore(cutoff int64) (int, error) {
	res, err := s.db.Exec(`DELETE FROM history WHERE ts < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
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

// MarkPeerOffline 把设备的最近发现时间清零，客户端据此立即判为离线。
func (s *Store) MarkPeerOffline(id string) error {
	_, err := s.db.Exec(`UPDATE peers SET last_seen=0 WHERE id=?`, id)
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
