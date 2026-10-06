package release

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// gramsMilli 以千分之一克为单位定点存储克数，避免浮点误差。
type gramsMilli int64

// maxGramsMilli 是同一批次内每种物料累计实投的上限，
// 即 9223372036854775.807 克（math.MaxInt64 个千分之一克）。
const maxGramsMilli gramsMilli = math.MaxInt64

// feedingAccumulator 按物料累计一个批次内的实投量（千分之一克）。
// 追加投料、读取台账校验与批次数量核对共用这一套数量规则：
// 累计范围始终是一个批次中的一种物料，其他物料与其他批次的数量
// 不参与，物料之间也不互相抵消；任一物料的累计上限为 maxGramsMilli，
// 恰好达到上限合法，再增加 0.001 克即判超限。累计不回绕、不截断、
// 不舍入，精确到千分之一克。
type feedingAccumulator struct {
	sums map[string]gramsMilli
}

func newFeedingAccumulator() *feedingAccumulator {
	return &feedingAccumulator{sums: map[string]gramsMilli{}}
}

// add 计入一条该物料的投料；计入后会超过上限时返回 false 且不计入，
// 已有累计保持不变。
func (a *feedingAccumulator) add(materialNo string, g gramsMilli) bool {
	sum := a.sums[materialNo]
	if g > maxGramsMilli-sum {
		return false
	}
	a.sums[materialNo] = sum + g
	return true
}

// total 返回该物料的累计实投量；无投料记录时为零。
func (a *feedingAccumulator) total(materialNo string) gramsMilli {
	return a.sums[materialNo]
}

// gramsPattern 是两种克数解析共用的写法规则：整数或带一至三位小数，
// 可选一个前导负号。它有意不接受前导正号、科学计数法与空小数部分。
var gramsPattern = regexp.MustCompile(`^(-?)(\d+)(?:\.(\d{1,3}))?$`)

// 共用核心区分三类失败原因，调用方据此各自拼出与用途相符的错误信息：
// 空字符串、写法不合法、超出范围；正数限制则在正数解析里另行判断。
var (
	errGramsEmpty  = errors.New("克数不能为空")
	errGramsSyntax = errors.New("克数写法不合法")
	errGramsRange  = errors.New("克数超出范围")
)

// parseGramsMagnitude 是正数解析与带符号解析共用的核心：两者对写法、精度
// 与数量上限的判断必须一致，差异只在各自能否接受零与负数——那层限制由
// parseGrams / parseSignedGrams 分别处理，不在这里重复。
//
// 共用规则：
//   - 前后空白按 TrimSpace 去掉后再判断；空字符串（含只有空白）非法；
//   - 接受整数与一至三位小数（100、100.5、0.001），四位小数（即使末位为
//     零）、科学计数法、带前导加号的写法一律非法；
//   - 数量精确到千分之一克，按整数解析后换算成 milli，不截短、不舍入；
//   - 绝对值以 maxGramsMilli（9223372036854775.807 克）为上限，恰好到达
//     边界合法，再大 0.001 克即拒绝，先做范围检查避免整数回绕把超长数字
//     变成较小的合法数量。
//
// 返回去掉空白后的原文、是否带负号与绝对值的千分之一克数，
// 错误为 errGramsEmpty / errGramsSyntax / errGramsRange 之一。
func parseGramsMagnitude(s string) (text string, negative bool, milli gramsMilli, err error) {
	text = strings.TrimSpace(s)
	if text == "" {
		return "", false, 0, errGramsEmpty
	}
	m := gramsPattern.FindStringSubmatch(text)
	if m == nil {
		return text, false, 0, errGramsSyntax
	}
	whole, perr := strconv.ParseInt(m[2], 10, 64)
	if perr != nil {
		return text, false, 0, errGramsSyntax
	}
	var frac int64
	if m[3] != "" {
		f := m[3]
		for len(f) < 3 {
			f += "0"
		}
		frac, perr = strconv.ParseInt(f, 10, 64)
		if perr != nil {
			return text, false, 0, errGramsSyntax
		}
	}
	// 先做溢出检查，避免整数回绕把超大克数变成较小的值。
	if whole > int64(maxGramsMilli)/1000 ||
		(whole == int64(maxGramsMilli)/1000 && frac > int64(maxGramsMilli)%1000) {
		return text, false, 0, errGramsRange
	}
	return text, m[1] == "-", gramsMilli(whole*1000 + frac), nil
}

// parseGrams 将克数字符串精确解析为千分之一克，用于登记配方每份用量与单次
// 投料等新提交：只接受正数，最小为 0.001 克，零与负数一律拒绝。
// 写法、精度与上限规则与 parseSignedGrams 共用（见 parseGramsMagnitude）。
func parseGrams(s string) (gramsMilli, error) {
	text, negative, milli, err := parseGramsMagnitude(s)
	// 旧正数解析的写法不接受负号：带符号输入在写法阶段即被拒，错误信息也
	// 沿用“必须为正数且最多三位小数”，即使其绝对值已超上限也先报写法。
	if negative {
		return 0, fmt.Errorf("克数 %q 不合法：必须为正数且最多三位小数", text)
	}
	switch {
	case errors.Is(err, errGramsEmpty):
		return 0, errGramsEmpty
	case errors.Is(err, errGramsSyntax):
		return 0, fmt.Errorf("克数 %q 不合法：必须为正数且最多三位小数", text)
	case errors.Is(err, errGramsRange):
		return 0, fmt.Errorf("克数 %q 超出范围：单次数量不能超过 %s 克", text, maxGramsMilli)
	}
	if milli <= 0 {
		return 0, fmt.Errorf("克数 %q 必须为正数", text)
	}
	return milli, nil
}

// parseSignedGrams 将允许零与负数的克数字符串精确解析为千分之一克。
// 用于读取核对中比较应投量、实投量与差额：实投可以为零，差额可以为负，
// 这些都不是非法数量；但应投量与实投量仍必须非负，那层限制由各批次核对
// 调用方按该批次原有的核对要求另行判断，不能在这里把负差额一起拒绝。
// 写法、精度与绝对值上限规则与 parseGrams 共用（见 parseGramsMagnitude）：
// 只多允许一个前导负号与零值，1.000 与 1 是同一数量，绝对值同样不超过
// maxGramsMilli，恰好到达边界可以读取，超过 0.001 克即拒绝。
func parseSignedGrams(s string) (gramsMilli, error) {
	text, negative, milli, err := parseGramsMagnitude(s)
	switch {
	case errors.Is(err, errGramsEmpty):
		return 0, errGramsEmpty
	case errors.Is(err, errGramsSyntax):
		return 0, fmt.Errorf("克数 %q 不合法：最多三位小数", text)
	case errors.Is(err, errGramsRange):
		return 0, fmt.Errorf("克数 %q 超出范围：不能超过 %s 克", text, maxGramsMilli)
	}
	if negative {
		milli = -milli
	}
	return milli, nil
}

// String 将千分之一克格式化为克数字符串，去掉末尾多余的 0。
func (g gramsMilli) String() string {
	sign := ""
	v := int64(g)
	if v < 0 {
		sign = "-"
		v = -v
	}
	w, f := v/1000, v%1000
	if f == 0 {
		return sign + strconv.FormatInt(w, 10)
	}
	fs := strings.TrimRight(fmt.Sprintf("%03d", f), "0")
	return sign + strconv.FormatInt(w, 10) + "." + fs
}

// multiplyPortions 将每份克数与计划份数相乘，计算某物料的应投量
// （每份克数 × 计划份数）。它是数量规则 planRequirements 使用的底层
// 定点运算，创建批次、调整草稿、读取校验与批次数量核对最终都经由
// planRequirements 调用到这里：
//   - 计划份数必须为正整数——份数为零或负数的已保存批次即数据损坏，
//     也不能据此算出零或负的应投量；
//   - 相乘结果必须能用千分之一克（int64 个 milli）精确表示：克数本身
//     已是千分之一克的整数倍，整数相乘不引入更小的单位，不存在舍入；
//     结果超过 maxGramsMilli（9223372036854775.807 克）时返回错误。
//
// 这里只做一种物料一次乘法，按物料分别判断上限的职责在 planRequirements：
// 不与同批次其他物料的应投量相加；恰好等于上限合法，再大即溢出。
// 计算不回绕、不截断、不舍入。
func multiplyPortions(g gramsMilli, portions int) (gramsMilli, error) {
	if portions <= 0 {
		return 0, errors.New("份数必须为正整数")
	}
	v := int64(g)
	if v > (1<<63-1)/int64(portions) {
		return 0, fmt.Errorf("应投量超出上限 %s 克：%s 克 × %d 份无法精确表示",
			maxGramsMilli, g, portions)
	}
	return gramsMilli(v * int64(portions)), nil
}
