package release

import (
	"encoding/json"
	"errors"
	"time"
)

// BatchStatus 表示批次状态。
type BatchStatus string

const (
	// StatusDraft 草稿：可以调整计划份数或改选配方版本。
	StatusDraft BatchStatus = "draft"
	// StatusExecuting 执行中：份数与配方已固定，可追加投料。
	StatusExecuting BatchStatus = "executing"
	// StatusClosed 已关闭：确认已有投料，不可再追加或重新打开。
	StatusClosed BatchStatus = "closed"
)

// MaterialInput 是登记配方时的物料输入。
type MaterialInput struct {
	// MaterialNo 物料编号，同一配方版本内不可重复。
	MaterialNo string
	// Grams 生产一份该物料需要的克数，必须为正数且最多三位小数。
	Grams string
}

// MaterialView 是配方物料的查询视图。
type MaterialView struct {
	MaterialNo string
	Grams      string
}

// RecipeView 是配方版本的查询视图。
type RecipeView struct {
	RecipeNo  string
	Version   string
	Name      string
	Materials []MaterialView
}

// FeedingView 是一条投料记录的查询视图。
type FeedingView struct {
	Seq        int       // 投料登记顺序，从 1 开始
	MaterialNo string    // 物料编号（必然属于该批次绑定的配方版本）
	Grams      string    // 本次投料克数
	Time       time.Time // 投料时间（由调用方填写，不参与排序）
	Registrar  string    // 登记人
}

// MaterialRequirement 是按物料逐项列出的数量核对结果。
type MaterialRequirement struct {
	MaterialNo      string
	RequiredGrams   string // 应投量 = 每份克数 × 计划份数
	ActualGrams     string // 累计实投量（仅统计该物料的投料）
	DifferenceGrams string // 实投减应投（可能为负；物料之间不互相抵消）
}

// BatchView 是批次查询视图。
type BatchView struct {
	BatchNo         string
	RecipeNo        string
	RecipeVersion   string
	RecipeName      string
	PlannedPortions int
	Status          BatchStatus
	Feedings        []FeedingView         // 按成功登记的先后顺序排列
	Materials       []MaterialRequirement // 配方中每种物料一项，无投料也显示
}

// 导出的错误。调用方可以用 errors.Is 判定类别，
// 具体原因会通过 fmt.Errorf("%w: ...") 附带在错误信息中。
var (
	// ErrNotFound 查询的批次或配方版本不存在。
	ErrNotFound = errors.New("未找到")
	// ErrDuplicateBatch 批次编号已存在。
	ErrDuplicateBatch = errors.New("批次编号重复")
	// ErrRecipeExists 配方版本已登记，内容不可覆盖。
	ErrRecipeExists = errors.New("配方版本已存在，不可覆盖")
	// ErrInvalidInput 输入不合法（编号为空、克数非法、份数非正整数等）。
	ErrInvalidInput = errors.New("输入不合法")
	// ErrInvalidState 当前批次状态不允许该操作。
	ErrInvalidState = errors.New("当前状态不允许该操作")
	// ErrMaterialNotInRecipe 投料物料不属于该批次绑定的配方版本。
	ErrMaterialNotInRecipe = errors.New("物料不属于该批次的配方版本")
	// ErrRequestConflict 请求编号已被用于其他操作或不同内容。
	ErrRequestConflict = errors.New("请求编号冲突")
	// ErrCorruptData 台账数据已损坏或无法读取，不能当成空台账继续。
	ErrCorruptData = errors.New("台账数据损坏，无法读取")
)

// 内部持久化记录。

type materialRecord struct {
	MaterialNo string     `json:"materialNo"`
	GramsMilli gramsMilli `json:"gramsMilli"`
}

type recipeRecord struct {
	RecipeNo  string           `json:"recipeNo"`
	Version   string           `json:"version"`
	Name      string           `json:"name"`
	Materials []materialRecord `json:"materials"`
}

type feedingRecord struct {
	Seq        int        `json:"seq"`
	MaterialNo string     `json:"materialNo"`
	GramsMilli gramsMilli `json:"gramsMilli"`
	Time       time.Time  `json:"time"`
	Registrar  string     `json:"registrar"`
}

type batchRecord struct {
	BatchNo         string          `json:"batchNo"`
	RecipeNo        string          `json:"recipeNo"`
	RecipeVersion   string          `json:"recipeVersion"`
	PlannedPortions int             `json:"plannedPortions"`
	Status          BatchStatus     `json:"status"`
	Feedings        []feedingRecord `json:"feedings"`
}

type requestRecord struct {
	Op      string          `json:"op"`
	Payload string          `json:"payload"`
	Result  json.RawMessage `json:"result"`
}

type persistedState struct {
	Version  int                       `json:"version"`
	Recipes  []*recipeRecord           `json:"recipes"`
	Batches  []*batchRecord            `json:"batches"`
	Requests map[string]*requestRecord `json:"requests"`
}
