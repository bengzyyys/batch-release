package release

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

const (
	stateFileName = "ledger.json"
	lockFileName  = "ledger.lock"
	stateVersion  = 1
)

// Store 是一个本地配方批次台账，数据保存在调用方指定的目录中。
// 不同目录互相独立；多个 Store 可以安全并发提交（进程内互斥、进程间文件锁）。
type Store struct {
	dir      string
	lockFile *os.File
	mu       sync.Mutex
	state    *persistedState
}

// Open 打开（或首次使用）位于 dir 的台账。
// 目录不存在时会创建；目录中没有台账文件时得到空台账；
// 已有台账文件但无法读取或解析时返回 ErrCorruptData，不会当成空台账继续保存。
func Open(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("数据位置不能为空")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建数据位置 %q 失败: %w", dir, err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("读取数据位置 %q 失败: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("数据位置 %q 不是目录", dir)
	}

	lockPath := filepath.Join(dir, lockFileName)
	lf, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("打开锁文件失败: %w", err)
	}

	s := &Store{dir: dir, lockFile: lf}
	if err := s.load(); err != nil {
		lf.Close()
		return nil, err
	}
	return s, nil
}

// Close 释放台账占用的文件锁。
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lockFile == nil {
		return nil
	}
	err := s.lockFile.Close()
	s.lockFile = nil
	return err
}

func (s *Store) lockExclusive() error {
	if s.lockFile == nil {
		return errors.New("台账已关闭")
	}
	if err := syscall.Flock(int(s.lockFile.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("锁定台账失败: %w", err)
	}
	return nil
}

func (s *Store) lockShared() error {
	if s.lockFile == nil {
		return errors.New("台账已关闭")
	}
	if err := syscall.Flock(int(s.lockFile.Fd()), syscall.LOCK_SH); err != nil {
		return fmt.Errorf("锁定台账失败: %w", err)
	}
	return nil
}

func (s *Store) unlock() {
	_ = syscall.Flock(int(s.lockFile.Fd()), syscall.LOCK_UN)
}

func (s *Store) load() error {
	data, err := os.ReadFile(filepath.Join(s.dir, stateFileName))
	if err != nil {
		if os.IsNotExist(err) {
			s.state = &persistedState{Version: stateVersion, Requests: map[string]*requestRecord{}}
			return nil
		}
		return fmt.Errorf("%w: 读取台账文件失败: %v", ErrCorruptData, err)
	}
	if len(data) == 0 {
		s.state = &persistedState{Version: stateVersion, Requests: map[string]*requestRecord{}}
		return nil
	}
	var st persistedState
	if err := json.Unmarshal(data, &st); err != nil {
		return fmt.Errorf("%w: 台账文件解析失败: %v", ErrCorruptData, err)
	}
	if st.Version != stateVersion {
		return fmt.Errorf("%w: 台账版本 %d 不受支持", ErrCorruptData, st.Version)
	}
	if st.Requests == nil {
		st.Requests = map[string]*requestRecord{}
	}
	s.state = &st
	return nil
}

// persist 先写临时文件再原子改名，并 fsync 目录，保证已提交数据可恢复。
func (s *Store) persist(st *persistedState) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmpPath := filepath.Join(s.dir, stateFileName+".tmp")
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, filepath.Join(s.dir, stateFileName)); err != nil {
		return err
	}
	if d, err := os.Open(s.dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func cloneState(st *persistedState) (*persistedState, error) {
	data, err := json.Marshal(st)
	if err != nil {
		return nil, err
	}
	var out persistedState
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	if out.Requests == nil {
		out.Requests = map[string]*requestRecord{}
	}
	return &out, nil
}

// write 是所有写入操作的统一入口：
//   - reqNo 为空直接报错；
//   - 同一 reqNo 且操作与内容完全相同，返回第一次成功的结果（幂等重放）；
//   - 同一 reqNo 用于其他操作或不同内容，返回 ErrRequestConflict；
//   - apply 在状态副本上执行，返回校验错误时不落盘、不占用请求编号；
//   - apply 成功后，状态与请求记录一起原子落盘，再切换到内存状态。
func (s *Store) write(reqNo, op string, payload any, apply func(*persistedState) (json.RawMessage, error), out any) error {
	if reqNo == "" {
		return errors.New("请求编号不能为空")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.lockExclusive(); err != nil {
		return err
	}
	defer s.unlock()

	// 跨进程场景下，其他进程可能已提交新数据；加锁后重新加载，
	// 确保在最新状态上应用变更，避免用旧快照覆盖其他进程的写入。
	if err := s.load(); err != nil {
		return err
	}

	payloadRaw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	payloadStr := string(payloadRaw)

	if req, ok := s.state.Requests[reqNo]; ok {
		if req.Op != op || req.Payload != payloadStr {
			return fmt.Errorf("%w: 请求编号 %q 已用于操作 %q，不能再用于操作 %q 或不同内容",
				ErrRequestConflict, reqNo, req.Op, op)
		}
		if err := json.Unmarshal(req.Result, out); err != nil {
			return fmt.Errorf("重放请求 %q 的结果失败: %w", reqNo, err)
		}
		return nil
	}

	clone, err := cloneState(s.state)
	if err != nil {
		return err
	}
	result, err := apply(clone)
	if err != nil {
		return err
	}
	clone.Requests[reqNo] = &requestRecord{Op: op, Payload: payloadStr, Result: result}
	if err := s.persist(clone); err != nil {
		return fmt.Errorf("保存台账失败: %w", err)
	}
	s.state = clone
	if err := json.Unmarshal(result, out); err != nil {
		return fmt.Errorf("解析请求结果失败: %w", err)
	}
	return nil
}

// read 在共享锁下读取当前状态。
func (s *Store) read(fn func(*persistedState) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.lockShared(); err != nil {
		return err
	}
	defer s.unlock()
	if err := s.load(); err != nil {
		return err
	}
	return fn(s.state)
}

// 视图构造：每次都从记录全新构造，返回的切片与内部状态完全隔离。

func buildRecipeView(r *recipeRecord) *RecipeView {
	mats := make([]MaterialView, 0, len(r.Materials))
	for _, m := range r.Materials {
		mats = append(mats, MaterialView{MaterialNo: m.MaterialNo, Grams: m.GramsMilli.String()})
	}
	return &RecipeView{RecipeNo: r.RecipeNo, Version: r.Version, Name: r.Name, Materials: mats}
}

func buildBatchView(b *batchRecord, r *recipeRecord) (*BatchView, error) {
	v := &BatchView{
		BatchNo:         b.BatchNo,
		RecipeNo:        r.RecipeNo,
		RecipeVersion:   r.Version,
		RecipeName:      r.Name,
		PlannedPortions: b.PlannedPortions,
		Status:          b.Status,
		Feedings:        make([]FeedingView, 0, len(b.Feedings)),
		Materials:       make([]MaterialRequirement, 0, len(r.Materials)),
	}

	for _, f := range b.Feedings {
		v.Feedings = append(v.Feedings, FeedingView{
			Seq:        f.Seq,
			MaterialNo: f.MaterialNo,
			Grams:      f.GramsMilli.String(),
			Time:       f.Time,
			Registrar:  f.Registrar,
		})
	}

	for _, m := range r.Materials {
		required, err := multiplyPortions(m.GramsMilli, b.PlannedPortions)
		if err != nil {
			return nil, fmt.Errorf("批次 %q 数量核对失败: %w", b.BatchNo, err)
		}
		act, ok := sumMaterialGrams(b, m.MaterialNo)
		if !ok {
			return nil, fmt.Errorf("%w: 批次 %q 物料 %q 的累计实投超出可表示范围",
				ErrCorruptData, b.BatchNo, m.MaterialNo)
		}
		v.Materials = append(v.Materials, MaterialRequirement{
			MaterialNo:      m.MaterialNo,
			RequiredGrams:   required.String(),
			ActualGrams:     act.String(),
			DifferenceGrams: (act - required).String(),
		})
	}
	return v, nil
}

func findRecipe(st *persistedState, recipeNo, version string) *recipeRecord {
	for _, r := range st.Recipes {
		if r.RecipeNo == recipeNo && r.Version == version {
			return r
		}
	}
	return nil
}

func findBatch(st *persistedState, batchNo string) *batchRecord {
	for _, b := range st.Batches {
		if b.BatchNo == batchNo {
			return b
		}
	}
	return nil
}

// sumMaterialGrams 统计批次内某物料已登记投料的累计千分之一克数。
// 仅统计该物料的记录，其他物料不参与；返回 ok=false 表示仅已有记录
// 的合计就已经超过可表示上限（AddFeeding 的登记前校验会阻止这种状态，
// 这里仍防一手整数回绕）。
func sumMaterialGrams(b *batchRecord, materialNo string) (sum gramsMilli, ok bool) {
	for _, f := range b.Feedings {
		if f.MaterialNo != materialNo {
			continue
		}
		if f.GramsMilli > maxGramsMilli-sum {
			return maxGramsMilli, false
		}
		sum += f.GramsMilli
	}
	return sum, true
}
