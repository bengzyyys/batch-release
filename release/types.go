package release

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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

type persistedState struct {
	Version  int              `json:"version"`
	Recipes  []*recipeRecord  `json:"recipes"`
	Batches  []*batchRecord   `json:"batches"`
	Requests requestRecordMap `json:"requests"`
}

// requestsFieldName 是台账最外层请求记录字段的规范名称，错误信息始终写出
// 这个名字，与文件里实际使用的大小写或转义写法无关。
const requestsFieldName = "requests"

// isRequestsField 判断一个解码后的最外层 JSON 键是否指向请求记录字段。
// 标准库 encoding/json 选择结构体字段时用 foldName 比较键与字段标签，而
// foldName 的折叠结果与 bytes.EqualFold 等价（ASCII 转大写，非 ASCII 走
// unicode.SimpleFold），因此 "requests"、"Requests"、"REQUESTS" 以及
// 经 Unicode 简单大小写折叠后等价的写法（如长 s ſ 折叠成 s）都会写入同一
// 个字段；把字符写成 JSON 的码位转义（如 q 写成其转义）时，键字符串解码
// 后再参与同一折叠。这里直接用 strings.EqualFold 复现这条规则，使“标准
// 库会把该键写入 requests”与“该键算作请求记录字段”严格一致：标准库会
// 静默覆盖的写法一定被拦截，标准库当作未知字段忽略的写法（如西里尔字母
// 同音字形）也不会被误判成重复。
func isRequestsField(key string) bool {
	return strings.EqualFold(key, requestsFieldName)
}

// UnmarshalJSON 解析整份台账时，保证最外层请求记录字段只出现一次。
//
// 标准库把 JSON 对象读进结构体时，重复的字段名会静默地用后出现的值覆盖
// 先出现的值（只会在严格开启 DisallowUnknownFields 等场景报告，且也不
// 按数据损坏分类）：台账最外层若写了两次 requests，前一段保存的成功请求
// 结果会被后一段整段覆盖丢失，而已有投料仍在；调用方按原投料请求重放时
// 会被当成新登记，再增加一条投料。这里在标准解析之前先逐键扫描最外层，
// 请求记录字段出现第二次即以 ErrCorruptData 拒绝整份台账——无论两段内容
// 完全相同、各自保存不同编号，还是其中一段为空对象或 null，都不能挑选、
// 拼接两段或凭台账里的其他记录重建请求结果。
//
// 只有最外层对象自身的键参与判断：请求记录内部（或任何嵌套对象里）出现
// 同名字段不算最外层重复，正常数据不会因此被拒绝。扫描时只跳过各字段的
// 值、不深入其结构，随后仍委托标准解析得到完整台账（requests 对象内部
// 同编号重复由 requestRecordMap.UnmarshalJSON 另行拒绝）。
func (st *persistedState) UnmarshalJSON(data []byte) error {
	if err := rejectDuplicateRequestsField(data); err != nil {
		return err
	}
	// 委托标准解析：逐字段填充结构体，并继续走 requestRecordMap 的
	// UnmarshalJSON 检查 requests 对象内部的重复请求编号。为避免本方法
	// 递归调用，使用一个字段与 persistedState 完全一致、但没有自定义
	// UnmarshalJSON 的别名类型。
	type plain persistedState
	var out plain
	if err := json.Unmarshal(data, &out); err != nil {
		return err
	}
	*st = persistedState(out)
	return nil
}

// rejectDuplicateRequestsField 用 JSON 流扫描台账最外层对象：请求记录字段
// （含大小写变体与 Unicode 转义写法）出现第二次即返回包装 ErrCorruptData
// 的错误。只检查最外层这一层，各字段值被整体跳过，不进入嵌套结构。
func rejectDuplicateRequestsField(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	d, ok := tok.(json.Delim)
	if !ok || d != '{' {
		// 顶层不是对象时交给标准解析报告 JSON 类型错误。
		return nil
	}
	seen := false
	for dec.More() {
		ktok, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := ktok.(string)
		if !ok {
			// 非字符串键不属于合法 JSON 对象，交给标准解析拒绝。
			return nil
		}
		// 读到键的这一刻就判定：第二次出现请求记录字段立即拒绝，与其值
		// 能否解析、是空对象还是 null 无关。
		if isRequestsField(key) {
			if seen {
				return fmt.Errorf("%w: 台账最外层请求记录字段 %q 重复：同一份台账中 %s 字段只能出现一次，不能挑选、拼接两段或重建请求结果",
					ErrCorruptData, key, requestsFieldName)
			}
			seen = true
		}
		// 整体跳过该字段的值（对象、数组、标量都由 Decode 消费），
		// 不深入嵌套对象——记录内部出现同名字段不算最外层重复。
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return err
		}
	}
	if _, err := dec.Token(); err != nil { // 收尾的 '}'
		return err
	}
	return nil
}
