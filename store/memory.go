// memory.go —— Store 的**内存参考实现**（不依赖任何数据库）。
//
// 用途：
//   - 组件单测（无需 bbolt / 临时目录 / 文件清理）；
//   - 演示「会话组件不关心怎么存」——换一个 Store 实现即可落 Oracle / 单库 / 远端。
//
// 语义与 bbolt 实现对齐：记录按 JSON 编解码（读出的数字为 float64，与 data 侧一致）、
// 主键附加 KeyField、事务原子（失败回滚）、Query 支持等值过滤 + 单字段排序 + Offset/Limit。
// 不支持 Cursor（返回明确错误）。
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Memory 是内存版 Store（并发安全）。
type Memory struct {
	mu     sync.Mutex
	spaces map[string]map[string]map[string]Record // dialogID(""=catalog) → table → key → row
}

// NewMemory 构造内存 Store（已含空的目录库）。
func NewMemory() *Memory {
	return &Memory{spaces: map[string]map[string]map[string]Record{"": {}}}
}

// ── Store ──────────────────────────────────────────────────────

// Catalog 返回目录库句柄（release 为空操作——内存实现无需引用计数）。
func (m *Memory) Catalog() (DB, func(), error) { return &memDB{m: m, space: ""}, func() {}, nil }

// OpenDialog 打开（不存在则创建）对话库。
func (m *Memory) OpenDialog(dialogID string) (DB, func(), error) {
	if dialogID == "" {
		return nil, nil, fmt.Errorf("store: dialog_id required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureSpace(dialogID)
	return &memDB{m: m, space: dialogID}, func() {}, nil
}

// ExistingDialog 打开已存在的对话库（缺失 → 错误）。
func (m *Memory) ExistingDialog(dialogID string) (DB, func(), error) {
	if dialogID == "" {
		return nil, nil, fmt.Errorf("store: dialog_id required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.spaces[dialogID]; !ok {
		return nil, nil, fmt.Errorf("store: 对话库不存在（dialog_id=%s）", dialogID)
	}
	return &memDB{m: m, space: dialogID}, func() {}, nil
}

// CloneDialog 深拷贝源对话库（目标已存在 → ErrForkTargetExists）。
func (m *Memory) CloneDialog(srcDialogID, dstDialogID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	src, ok := m.spaces[srcDialogID]
	if !ok {
		return fmt.Errorf("store: 源对话库不存在（dialog_id=%s）", srcDialogID)
	}
	if _, exists := m.spaces[dstDialogID]; exists {
		return fmt.Errorf("%w: %s", ErrForkTargetExists, dstDialogID)
	}
	cp := make(map[string]map[string]Record, len(src))
	for table, rows := range src {
		cpRows := make(map[string]Record, len(rows))
		for k, rec := range rows {
			cpRows[k] = copyRecord(rec)
		}
		cp[table] = cpRows
	}
	m.spaces[dstDialogID] = cp
	return nil
}

// DeleteDialog 删除对话库（幂等）。
func (m *Memory) DeleteDialog(dialogID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.spaces, dialogID)
	return nil
}

// DialogFileName 目录行 `file` 值（命名规则由本实现决定）。
func (m *Memory) DialogFileName(dialogID string) string { return "mem:" + dialogID }

// NewID 生成 UUID v4 文本。
func (m *Memory) NewID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("store: 生成 id: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32], nil
}

func (m *Memory) ensureSpace(dialogID string) map[string]map[string]Record {
	sp, ok := m.spaces[dialogID]
	if !ok {
		sp = map[string]map[string]Record{}
		m.spaces[dialogID] = sp
	}
	return sp
}

// ── DB ────────────────────────────────────────────────────────

type memDB struct {
	m     *Memory
	space string
}

func (d *memDB) Path() string { return d.space }

func (d *memDB) Table(name string) Table { return &memTable{m: d.m, space: d.space, name: name} }

// Update 在全局锁内执行（原子）；失败按 undo 回滚。
func (d *memDB) Update(fn func(tx Tx) error) error {
	d.m.mu.Lock()
	defer d.m.mu.Unlock()
	tx := &memTx{m: d.m, space: d.space}
	if err := fn(tx); err != nil {
		tx.rollback()
		return err
	}
	return nil
}

// ── Table ─────────────────────────────────────────────────────

type memTable struct {
	m     *Memory
	space string
	name  string
}

func (t *memTable) Get(key string, out *Record) (bool, error) {
	t.m.mu.Lock()
	defer t.m.mu.Unlock()
	rec, ok := t.m.ensureSpace(t.space)[t.name][key]
	if !ok {
		return false, nil
	}
	cp := copyRecord(rec)
	cp[KeyField] = key
	if out != nil {
		*out = cp
	}
	return true, nil
}

func (t *memTable) Insert(key string, rec Record) error {
	return t.m.putRow(t.space, t.name, key, rec, true)
}

func (t *memTable) Upsert(key string, rec Record) error {
	cp := copyRecord(rec)
	if _, has := cp["updated_at"]; !has {
		cp["updated_at"] = time.Now().UTC().Format(RFC3339FixedNano)
	}
	return t.m.putRow(t.space, t.name, key, cp, false)
}

func (t *memTable) UpdateIn(key string, fn func(rec Record) Record) error {
	t.m.mu.Lock()
	defer t.m.mu.Unlock()
	rows := t.m.ensureSpace(t.space)[t.name]
	if rows == nil {
		rows = map[string]Record{}
		t.m.spaces[t.space][t.name] = rows
	}
	cur := Record{}
	if old, ok := rows[key]; ok {
		cur = copyRecord(old)
	}
	out := fn(cur)
	if out == nil {
		return nil
	}
	rows[key] = copyRecord(out)
	return nil
}

func (t *memTable) Delete(key string) error {
	t.m.mu.Lock()
	defer t.m.mu.Unlock()
	rows := t.m.ensureSpace(t.space)[t.name]
	if _, ok := rows[key]; !ok {
		return ErrNotFound
	}
	delete(rows, key)
	return nil
}

func (t *memTable) ForEach(fn func(key string, rec Record) error) error {
	t.m.mu.Lock()
	defer t.m.mu.Unlock()
	rows := t.m.ensureSpace(t.space)[t.name]
	keys := make([]string, 0, len(rows))
	for k := range rows {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		cp := copyRecord(rows[k])
		cp[KeyField] = k
		if err := fn(k, cp); err != nil {
			return err
		}
	}
	return nil
}

func (t *memTable) Query(q Query) ([]Record, string, error) {
	if q.Cursor != "" {
		return nil, "", fmt.Errorf("store: 内存实现不支持 Cursor")
	}
	var all []Record
	err := t.ForEach(func(_ string, rec Record) error {
		if len(q.Where) > 0 && !matchWhere(rec, q.Where) {
			return nil
		}
		all = append(all, rec)
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	if q.OrderBy != "" {
		sort.SliceStable(all, func(i, j int) bool {
			ai := fieldString(all[i], q.OrderBy)
			bi := fieldString(all[j], q.OrderBy)
			if q.OrderDesc {
				return ai > bi
			}
			return ai < bi
		})
	}
	start := q.Offset
	if start < 0 {
		start = 0
	}
	if start > len(all) {
		start = len(all)
	}
	end := len(all)
	if q.Limit > 0 && start+q.Limit < end {
		end = start + q.Limit
	}
	return all[start:end], "", nil
}

// putRow 是 Insert/Upsert 的共用写入（insertOnly=true 时已存在 → ErrExists）。
func (m *Memory) putRow(space, table, key string, rec Record, insertOnly bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	sp := m.ensureSpace(space)
	rows, ok := sp[table]
	if !ok {
		rows = map[string]Record{}
		sp[table] = rows
	}
	if insertOnly {
		if _, exists := rows[key]; exists {
			return ErrExists
		}
	}
	rows[key] = copyRecord(rec)
	return nil
}

// ── Tx ────────────────────────────────────────────────────────

type memTx struct {
	m     *Memory
	space string
	undo  []func()
}

func (tx *memTx) rollback() {
	for i := len(tx.undo) - 1; i >= 0; i-- {
		tx.undo[i]()
	}
}

func (tx *memTx) rows(table string) map[string]Record {
	sp := tx.m.ensureSpace(tx.space)
	rows, ok := sp[table]
	if !ok {
		rows = map[string]Record{}
		sp[table] = rows
	}
	return rows
}

// remember 记录某 (table,key) 的旧值，供回滚恢复。
func (tx *memTx) remember(table, key string) {
	rows := tx.rows(table)
	old, had := rows[key]
	tx.undo = append(tx.undo, func() {
		sp := tx.m.ensureSpace(tx.space)
		if had {
			cp := copyRecord(old)
			sp[table][key] = cp
			return
		}
		delete(sp[table], key)
	})
}

func (tx *memTx) Get(table, key string, out *Record) (bool, error) {
	rec, ok := tx.rows(table)[key]
	if !ok {
		return false, nil
	}
	cp := copyRecord(rec)
	cp[KeyField] = key
	if out != nil {
		*out = cp
	}
	return true, nil
}

func (tx *memTx) Insert(table, key string, rec Record) error {
	rows := tx.rows(table)
	if _, exists := rows[key]; exists {
		return ErrExists
	}
	tx.remember(table, key)
	rows[key] = copyRecord(rec)
	return nil
}

func (tx *memTx) Put(table, key string, rec Record) error {
	tx.remember(table, key)
	tx.rows(table)[key] = copyRecord(rec)
	return nil
}

func (tx *memTx) Delete(table, key string) error {
	rows := tx.rows(table)
	if _, ok := rows[key]; !ok {
		return ErrNotFound
	}
	tx.remember(table, key)
	delete(rows, key)
	return nil
}

func (tx *memTx) ForEach(table string, fn func(key string, rec Record) error) error {
	rows := tx.rows(table)
	keys := make([]string, 0, len(rows))
	for k := range rows {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		cp := copyRecord(rows[k])
		cp[KeyField] = k
		if err := fn(k, cp); err != nil {
			return err
		}
	}
	return nil
}

// ── 工具 ───────────────────────────────────────────────────────

// copyRecord 深拷贝一行（JSON 往返：与 bbolt 实现的解码类型一致）。
func copyRecord(rec Record) Record {
	if rec == nil {
		return Record{}
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		out := make(Record, len(rec))
		for k, v := range rec {
			if k != KeyField {
				out[k] = v
			}
		}
		return out
	}
	var out Record
	if err := json.Unmarshal(raw, &out); err != nil || out == nil {
		out = Record{}
	}
	delete(out, KeyField)
	return out
}

func matchWhere(rec Record, where map[string]any) bool {
	for k, want := range where {
		got, ok := lookup(rec, k)
		if !ok || !equalValue(got, want) {
			return false
		}
	}
	return true
}

func lookup(rec Record, path string) (any, bool) {
	cur := any(map[string]any(rec))
	for _, seg := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[seg]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

func equalValue(a, b any) bool {
	if af, ok := toFloat(a); ok {
		if bf, ok2 := toFloat(b); ok2 {
			return af == bf
		}
	}
	return reflect.DeepEqual(a, b)
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

func fieldString(rec Record, field string) string {
	v, ok := lookup(rec, field)
	if !ok || v == nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	default:
		return fmt.Sprintf("%v", x)
	}
}
