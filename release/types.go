package release

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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

// validBatchStatus 是已保存批次状态允许出现的全部取值，必须精确匹配。
// 读取台账时状态只能是这三个字符串之一：缺失、为空、null、其他字符串，
// 或大小写不同、前后多出空格的近似写法都不自动归入某个合法状态。
var validBatchStatus = map[BatchStatus]bool{
	StatusDraft:     true,
	StatusExecuting: true,
	StatusClosed:    true,
}

// isValidBatchStatus 判断已保存批次状态是否精确对应一个合法状态。
// 只做逐字符的精确匹配，不做大小写归一化或去空格："Draft"、" draft "
// 等写法与 "ready"、空字符串一样属于无法识别的状态。
func isValidBatchStatus(s BatchStatus) bool {
	return validBatchStatus[s]
}

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

// requestRecordMap 是台账 requests 对象的持久化类型：以请求编号为键的成功
// 请求记录表。同一请求编号在台账中只能有一份记录——标准库把 JSON 对象读进
// map 时，同名的重复键只会静默保留其中一份，另一份就此脱离一切核对：两份
// 保存的成功请求被写成同一个编号时，重放该编号可能返回后写入的那份结果，
// 而不是第一次成功的结果。因此这里按 JSON 流逐键读取，解码后的实际编号
// 出现第二次即以 ErrCorruptData 拒绝整份台账。
//
// 编号是否重复按 JSON 字符串解码后的实际内容判断：直接写出的字符与用
// Unicode 转义写法表示同一字符的写法（例如连字符直接写出与写成转义形式）
// 算同一个编号；不做去空格或忽略大小写等额外归一化，不同编号仍按精确匹配
// 区分。两份记录的操作、提交内容与返回结果完全相同也属于重复；只有
// requests 对象自身的键参与判断，各请求结果里同名的批次编号、物料编号等
// 字段与此无关。
type requestRecordMap map[string]*requestRecord

// UnmarshalJSON 逐键读取 requests 对象并拒绝重复请求编号。发现同一编号
// 第二次出现时返回包装 ErrCorruptData 的错误：不能挑第一份或最后一份继续
// 使用，不能合并记录或删除重复项，也不能重新计算结果后掩盖冲突。
func (m *requestRecordMap) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		*m = nil
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return fmt.Errorf("台账的 requests 必须是对象")
	}
	out := make(map[string]*requestRecord)
	for dec.More() {
		ktok, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := ktok.(string)
		if !ok {
			return fmt.Errorf("台账的 requests 包含非字符串的请求编号")
		}
		if _, dup := out[key]; dup {
			return fmt.Errorf("%w: 请求编号 %q 存在多份同编号请求记录，不能挑选、合并或删除其中一份后继续",
				ErrCorruptData, key)
		}
		var rec *requestRecord
		if err := dec.Decode(&rec); err != nil {
			return err
		}
		out[key] = rec
	}
	if _, err := dec.Token(); err != nil { // 收尾的 '}'
		return err
	}
	*m = out
	return nil
}

// requestsFieldName 是台账最外层请求记录字段的 JSON 名称，persistedState 中
// 的 Requests 与 requestsFieldProbe 都用这同一个标签绑定。
const requestsFieldName = "requests"

// requestsFieldProbe 只在把台账顶层对象正式解码进 persistedState 之前使用：
// 它不保存 requests 的内容，只统计最外层请求记录字段被标准库交付的次数。
//
// persistedState.Requests 是带自定义 UnmarshalJSON 的 map 类型，标准库把 JSON
// 对象解码到非指针字段时，最外层每出现一次 requests 就新建一个空 map 调一次
// UnmarshalJSON 后整体替换该字段——后一段会覆盖前一段，前一段里第一次登记
// 保存的请求结果就此脱离一切核对与重放；调用方再拿原投料请求提交时会被当成
// 新登记，平白增加一条投料。正式解码无法区分“字段出现了一次”与“出现多次、
// 后一段覆盖了前一段”，因此在正式解码前先用本探针按同样的字段匹配规则计数。
//
// 字段是否指向 requests 完全交给标准库按既有规则判定：JSON 字符串转义解码后
// 指向 requests 的写法（例如把其中字母 q 写成其 Unicode 转义的六个字符
// 反斜杠、u、0、0、7、1）与大小写折叠后相等的写法（如 Requests、
// REQUESTS）都会交付到这里，与正式解码、以及现有读取能识别的写法保持一致，
// 不会留下可用转义或大小写绕过的缺口；requests- 等近似名称不会命中。嵌套
// 对象（如某请求结果内部）里的同名字段不属于最外层，不会交付到顶层字段。
// 探针只计数、不校验各段内容：两段完全相同、各自保存不同编号，或其中一段
// 为空对象、null，第二次出现都同样拒绝。
type requestsFieldProbe struct {
	occurrences int
}

// UnmarshalJSON 在最外层请求记录字段每被交付一次时计数；第二次出现即返回
// ErrCorruptData，让整份台账在正式解码前被拒绝——不选择其中一段、不拼接
// 两段，也不重建任何请求结果。
func (p *requestsFieldProbe) UnmarshalJSON(data []byte) error {
	p.occurrences++
	if p.occurrences > 1 {
		return fmt.Errorf("%w: 台账最外层的请求记录字段 %q 重复出现：同一份台账的 %s 字段只能出现一次，第二次出现即按数据损坏处理，不能选择其中一段、拼接两段或重建请求结果",
			ErrCorruptData, requestsFieldName, requestsFieldName)
	}
	return nil
}

type persistedState struct {
	Version  int              `json:"version"`
	Recipes  []*recipeRecord  `json:"recipes"`
	Batches  []*batchRecord   `json:"batches"`
	Requests requestRecordMap `json:"requests"`
}
