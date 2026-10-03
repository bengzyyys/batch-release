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
// 已有台账文件无法读取、解析，任一配方版本的任一物料每份克数不是正数，
// 任一配方版本内同一物料编号出现多次，
// 任一批次绑定的配方版本未登记，
// 已保存投料的物料编号不属于该批次绑定的配方版本，
// 或已保存的投料数量非法（单条不是正数，或同一批次同一物料累计实投
// 超过 9223372036854775.807 克）时，返回 ErrCorruptData（可用 errors.Is
// 判断），不会当成空台账继续保存，也不会返回可继续使用的台账对象。
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
	if err := validateState(&st); err != nil {
		return err
	}
	if st.Requests == nil {
		st.Requests = map[string]*requestRecord{}
	}
	s.state = &st
	return nil
}

// validateState 检查台账的引用完整性与数量合法性：
//   - 每个配方版本的每种物料，每份克数必须是正数——与登记时的规则一致。
//     任一版本（包括尚未被任何批次使用的版本）的任一物料为零或负数，
//     整份台账即视为损坏：不能改成最小用量、取绝对值、删除问题物料
//     或改用其他版本后继续，其他配方完整或批次数量恰好吻合也不能放行。
//   - 同一配方版本的物料列表内，每个物料编号只能出现一次——与登记时的
//     规则一致。任一版本（包括尚未被任何批次使用的版本）内同一物料编号
//     出现多次，整份台账即视为损坏：不能合并重复项、相加克数、丢弃其中
//     一条或由查询者挑一项作为依据，即使两条克数完全相同也不能放行；
//     不同版本、不同配方各自使用同一物料编号不受影响。
//   - 每个批次记录的配方编号与版本号必须共同指向一个已登记的配方版本。
//     任一批次找不到对应版本，整份台账即视为损坏——不能改用同编号的其他版本，
//     也不能按名称或物料内容替换，即使其他批次仍然完整也不能放行本次读取。
//   - 每条已保存的投料数量必须是正数；同一批次内同一物料的累计实投
//     不得超过 maxGramsMilli（9223372036854775.807 克）。任一记录为零或
//     负数，或任一物料累计超限，整份台账即视为损坏——负数记录即使能被
//     正数抵消回范围内也不接受，超限也不能截断或忽略后继续。
//   - 每条已保存投料的物料编号必须属于该批次绑定的配方版本。归属以批次
//     实际绑定的版本为准：物料只出现在同编号的其他版本或其他配方中，
//     不能作为接受依据；也不能改选版本、补入物料或丢弃问题投料后继续。
func validateState(st *persistedState) error {
	for _, r := range st.Recipes {
		if r == nil {
			return fmt.Errorf("%w: 台账中存在空的配方记录", ErrCorruptData)
		}
		seen := make(map[string]bool, len(r.Materials))
		for _, m := range r.Materials {
			if seen[m.MaterialNo] {
				return fmt.Errorf("%w: 配方 %q 版本 %q 的物料编号 %q 重复",
					ErrCorruptData, r.RecipeNo, r.Version, m.MaterialNo)
			}
			seen[m.MaterialNo] = true
			if m.GramsMilli <= 0 {
				return fmt.Errorf("%w: 配方 %q 版本 %q 物料 %q 的每份克数不是正数",
					ErrCorruptData, r.RecipeNo, r.Version, m.MaterialNo)
			}
		}
	}
	for _, b := range st.Batches {
		if b == nil {
			return fmt.Errorf("%w: 台账中存在空的批次记录", ErrCorruptData)
		}
		r := findRecipe(st, b.RecipeNo, b.RecipeVersion)
		if r == nil {
			return fmt.Errorf("%w: 批次 %q 绑定的配方 %q 版本 %q 未登记",
				ErrCorruptData, b.BatchNo, b.RecipeNo, b.RecipeVersion)
		}
		if err := validateFeedings(b, r); err != nil {
			return err
		}
	}
	return nil
}

// validateFeedings 检查一个批次内已保存投料的合法性与配方归属。
// 归属以批次绑定的配方版本 r 为准：每条投料的物料编号必须在 r 的物料
// 列表中，否则整份台账视为损坏。数量方面只判断数量本身是否合法，不判断
// 实投是否符合配方：无投料的物料累计为零属正常，不足或超过应投量也不
// 在此拒绝。
func validateFeedings(b *batchRecord, r *recipeRecord) error {
	acc := newFeedingAccumulator()
	for _, f := range b.Feedings {
		belongs := false
		for _, m := range r.Materials {
			if m.MaterialNo == f.MaterialNo {
				belongs = true
				break
			}
		}
		if !belongs {
			return fmt.Errorf("%w: 批次 %q 的第 %d 条投料物料 %q 不属于其绑定的配方 %q 版本 %q",
				ErrCorruptData, b.BatchNo, f.Seq, f.MaterialNo, b.RecipeNo, b.RecipeVersion)
		}
		if f.GramsMilli <= 0 {
			return fmt.Errorf("%w: 批次 %q 物料 %q 的第 %d 条投料数量不是正数",
				ErrCorruptData, b.BatchNo, f.MaterialNo, f.Seq)
		}
		if !acc.add(f.MaterialNo, f.GramsMilli) {
			return fmt.Errorf("%w: 批次 %q 物料 %q 的累计实投超出上限 %s 克",
				ErrCorruptData, b.BatchNo, f.MaterialNo, maxGramsMilli)
		}
	}
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

	// 按物料累计实投量；台账在读取时已通过 validateFeedings 校验，
	// 这里仍用同一套累计规则防一手整数回绕。
	acc := newFeedingAccumulator()
	for _, f := range b.Feedings {
		if !acc.add(f.MaterialNo, f.GramsMilli) {
			return nil, fmt.Errorf("%w: 批次 %q 物料 %q 的累计实投超出可表示范围",
				ErrCorruptData, b.BatchNo, f.MaterialNo)
		}
	}

	// 数量核对按配方物料逐项列出；台账在读取时已通过 validateState 校验，
	// 这里仍防一手同一版本内物料编号重复，避免返回重复的数量核对项。
	seen := make(map[string]bool, len(r.Materials))
	for _, m := range r.Materials {
		if seen[m.MaterialNo] {
			return nil, fmt.Errorf("%w: 配方 %q 版本 %q 的物料编号 %q 重复",
				ErrCorruptData, r.RecipeNo, r.Version, m.MaterialNo)
		}
		seen[m.MaterialNo] = true
		required, err := multiplyPortions(m.GramsMilli, b.PlannedPortions)
		if err != nil {
			return nil, fmt.Errorf("批次 %q 数量核对失败: %w", b.BatchNo, err)
		}
		act := acc.total(m.MaterialNo)
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
