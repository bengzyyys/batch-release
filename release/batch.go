package release

import (
	"encoding/json"
	"fmt"
	"time"
)

const (
	opCreateBatch = "createBatch"
	opUpdateDraft = "updateDraftBatch"
	opStartBatch  = "startBatch"
	opCloseBatch  = "closeBatch"
	opAddFeeding  = "addFeeding"
)

type createBatchPayload struct {
	BatchNo  string
	RecipeNo string
	Version  string
	Portions int
}

type updateDraftPayload struct {
	BatchNo  string
	RecipeNo string // 留空表示不改配方
	Version  string // 与 RecipeNo 同时给出
	Portions int    // 0 表示不改份数
}

type startBatchPayload struct {
	BatchNo string
}

type closeBatchPayload struct {
	BatchNo string
}

type addFeedingPayload struct {
	BatchNo    string
	MaterialNo string
	Grams      string
	Time       time.Time
	Registrar  string
}

// CreateBatch 创建批次。
// 批次使用独立批次编号，选择已登记的配方版本和计划份数（必须为正整数）。
// 新批次为草稿；批次编号重复、配方版本不存在或数量不合法时返回具体原因且不保留数据。
func (s *Store) CreateBatch(reqNo, batchNo, recipeNo, version string, portions int) (*BatchView, error) {
	payload := createBatchPayload{BatchNo: batchNo, RecipeNo: recipeNo, Version: version, Portions: portions}
	var out BatchView
	err := s.write(reqNo, opCreateBatch, payload, func(st *persistedState) (json.RawMessage, error) {
		if batchNo == "" {
			return nil, fmt.Errorf("%w: 批次编号不能为空", ErrInvalidInput)
		}
		if portions <= 0 {
			return nil, fmt.Errorf("%w: 计划份数必须为正整数，得到 %d", ErrInvalidInput, portions)
		}
		if findBatch(st, batchNo) != nil {
			return nil, fmt.Errorf("%w: 批次 %q 已存在", ErrDuplicateBatch, batchNo)
		}
		r := findRecipe(st, recipeNo, version)
		if r == nil {
			return nil, fmt.Errorf("%w: 配方 %q 版本 %q 不存在", ErrNotFound, recipeNo, version)
		}
		b := &batchRecord{
			BatchNo:         batchNo,
			RecipeNo:        recipeNo,
			RecipeVersion:   version,
			PlannedPortions: portions,
			Status:          StatusDraft,
		}
		view, err := buildBatchView(b, r)
		if err != nil {
			return nil, err
		}
		raw, err := json.Marshal(view)
		if err != nil {
			return nil, err
		}
		st.Batches = append(st.Batches, b)
		return raw, nil
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateDraftBatch 调整草稿批次的计划份数或改选配方版本。
// newRecipeNo/newVersion 都留空表示不改配方；newPortions 传 0 表示不改份数。
// 只有草稿状态可以调整；开始执行后两者固定。
func (s *Store) UpdateDraftBatch(reqNo, batchNo, newRecipeNo, newVersion string, newPortions int) (*BatchView, error) {
	payload := updateDraftPayload{BatchNo: batchNo, RecipeNo: newRecipeNo, Version: newVersion, Portions: newPortions}
	var out BatchView
	err := s.write(reqNo, opUpdateDraft, payload, func(st *persistedState) (json.RawMessage, error) {
		b := findBatch(st, batchNo)
		if b == nil {
			return nil, fmt.Errorf("%w: 批次 %q", ErrNotFound, batchNo)
		}
		if b.Status != StatusDraft {
			return nil, fmt.Errorf("%w: 批次 %q 状态为 %s，只有草稿可以调整", ErrInvalidState, batchNo, b.Status)
		}
		if newPortions != 0 {
			if newPortions <= 0 {
				return nil, fmt.Errorf("%w: 计划份数必须为正整数，得到 %d", ErrInvalidInput, newPortions)
			}
			b.PlannedPortions = newPortions
		}
		if newRecipeNo != "" || newVersion != "" {
			if newRecipeNo == "" || newVersion == "" {
				return nil, fmt.Errorf("%w: 改选配方版本时必须同时给出配方编号和版本号", ErrInvalidInput)
			}
			r := findRecipe(st, newRecipeNo, newVersion)
			if r == nil {
				return nil, fmt.Errorf("%w: 配方 %q 版本 %q 不存在", ErrNotFound, newRecipeNo, newVersion)
			}
			b.RecipeNo = newRecipeNo
			b.RecipeVersion = newVersion
		}
		r := findRecipe(st, b.RecipeNo, b.RecipeVersion)
		view, err := buildBatchView(b, r)
		if err != nil {
			return nil, err
		}
		raw, err := json.Marshal(view)
		if err != nil {
			return nil, err
		}
		return raw, nil
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// StartBatch 开始执行批次：草稿 → 执行中。
// 开始后计划份数与配方版本固定，只有执行中的批次可以关闭。
func (s *Store) StartBatch(reqNo, batchNo string) (*BatchView, error) {
	payload := startBatchPayload{BatchNo: batchNo}
	var out BatchView
	err := s.write(reqNo, opStartBatch, payload, func(st *persistedState) (json.RawMessage, error) {
		b := findBatch(st, batchNo)
		if b == nil {
			return nil, fmt.Errorf("%w: 批次 %q", ErrNotFound, batchNo)
		}
		if b.Status != StatusDraft {
			return nil, fmt.Errorf("%w: 批次 %q 状态为 %s，只有草稿可以开始执行", ErrInvalidState, batchNo, b.Status)
		}
		b.Status = StatusExecuting
		r := findRecipe(st, b.RecipeNo, b.RecipeVersion)
		view, err := buildBatchView(b, r)
		if err != nil {
			return nil, err
		}
		raw, err := json.Marshal(view)
		if err != nil {
			return nil, err
		}
		return raw, nil
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// CloseBatch 关闭批次：执行中 → 已关闭。
// 关闭表示确认已有投料，不要求数量已经吻合；
// 关闭后不能追加投料，也不能重新打开。
func (s *Store) CloseBatch(reqNo, batchNo string) (*BatchView, error) {
	payload := closeBatchPayload{BatchNo: batchNo}
	var out BatchView
	err := s.write(reqNo, opCloseBatch, payload, func(st *persistedState) (json.RawMessage, error) {
		b := findBatch(st, batchNo)
		if b == nil {
			return nil, fmt.Errorf("%w: 批次 %q", ErrNotFound, batchNo)
		}
		if b.Status != StatusExecuting {
			return nil, fmt.Errorf("%w: 批次 %q 状态为 %s，只有执行中的批次可以关闭", ErrInvalidState, batchNo, b.Status)
		}
		b.Status = StatusClosed
		r := findRecipe(st, b.RecipeNo, b.RecipeVersion)
		view, err := buildBatchView(b, r)
		if err != nil {
			return nil, err
		}
		raw, err := json.Marshal(view)
		if err != nil {
			return nil, err
		}
		return raw, nil
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// AddFeeding 向执行中的批次追加一次投料。
// 投料包含物料编号、克数（正数、最多三位小数）、投料时间和登记人；
// 物料必须属于该批次绑定的配方版本。允许实际投料不足或超过应投量；
// 已登记的投料不能修改或删除，关闭后也不能追加。
// 同一批次内每种物料的累计实投量上限为 9223372036854775.807 克，
// 上限按物料分别判断，恰好等于上限允许；本次投料会使该物料累计超过上限时
// 返回 ErrInvalidInput（错误说明物料编号），不保存本次投料。
// 投料按成功登记的先后顺序编号，不按投料时间重排。
func (s *Store) AddFeeding(reqNo, batchNo, materialNo, grams string, t time.Time, registrar string) (*FeedingView, error) {
	payload := addFeedingPayload{
		BatchNo:    batchNo,
		MaterialNo: materialNo,
		Grams:      grams,
		Time:       t,
		Registrar:  registrar,
	}
	var out FeedingView
	err := s.write(reqNo, opAddFeeding, payload, func(st *persistedState) (json.RawMessage, error) {
		if materialNo == "" {
			return nil, fmt.Errorf("%w: 物料编号不能为空", ErrInvalidInput)
		}
		if registrar == "" {
			return nil, fmt.Errorf("%w: 登记人不能为空", ErrInvalidInput)
		}
		milli, err := parseGrams(grams)
		if err != nil {
			return nil, fmt.Errorf("%w: 克数: %v", ErrInvalidInput, err)
		}
		b := findBatch(st, batchNo)
		if b == nil {
			return nil, fmt.Errorf("%w: 批次 %q", ErrNotFound, batchNo)
		}
		if b.Status != StatusExecuting {
			return nil, fmt.Errorf("%w: 批次 %q 状态为 %s，只有执行中的批次可以投料", ErrInvalidState, batchNo, b.Status)
		}
		r := findRecipe(st, b.RecipeNo, b.RecipeVersion)
		if r == nil {
			return nil, fmt.Errorf("%w: 批次 %q 绑定的配方版本不存在", ErrCorruptData, batchNo)
		}
		belongs := false
		for _, m := range r.Materials {
			if m.MaterialNo == materialNo {
				belongs = true
				break
			}
		}
		if !belongs {
			return nil, fmt.Errorf("%w: 物料 %q 不在配方 %q 版本 %q 中",
				ErrMaterialNotInRecipe, materialNo, r.RecipeNo, r.Version)
		}

		// 上限按物料分别判断：只累计该物料已登记的投料，
		// 其他物料投了多少不影响本次登记；累计恰好等于上限允许，超过则拒绝。
		var used gramsMilli
		for _, f := range b.Feedings {
			if f.MaterialNo == materialNo {
				used += f.GramsMilli
			}
		}
		if milli > maxGramsMilli-used {
			return nil, fmt.Errorf("%w: 物料 %q 的累计投料数量超出范围：本次 %s 克，已累计 %s 克，上限 %s 克",
				ErrInvalidInput, materialNo, milli, used, maxGramsMilli)
		}

		seq := len(b.Feedings) + 1
		rec := feedingRecord{
			Seq:        seq,
			MaterialNo: materialNo,
			GramsMilli: milli,
			Time:       t,
			Registrar:  registrar,
		}
		view := FeedingView{
			Seq:        seq,
			MaterialNo: materialNo,
			Grams:      milli.String(),
			Time:       t,
			Registrar:  registrar,
		}
		raw, err := json.Marshal(view)
		if err != nil {
			return nil, err
		}
		b.Feedings = append(b.Feedings, rec)
		return raw, nil
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// GetBatch 按批次编号查询，返回未找到（ErrNotFound）时表示批次不存在。
// 返回内容包含所用配方版本、计划份数、状态、全部投料（登记顺序），
// 以及按物料逐项的应投量、累计实投量和差额；无投料的物料也会列出。
// 返回的视图是台账数据的副本，调用方修改不会影响台账。
func (s *Store) GetBatch(batchNo string) (*BatchView, error) {
	var out *BatchView
	err := s.read(func(st *persistedState) error {
		b := findBatch(st, batchNo)
		if b == nil {
			return fmt.Errorf("%w: 批次 %q", ErrNotFound, batchNo)
		}
		r := findRecipe(st, b.RecipeNo, b.RecipeVersion)
		if r == nil {
			return fmt.Errorf("%w: 批次 %q 绑定的配方 %q 版本 %q 不存在",
				ErrCorruptData, batchNo, b.RecipeNo, b.RecipeVersion)
		}
		view, err := buildBatchView(b, r)
		if err != nil {
			return err
		}
		out = view
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
