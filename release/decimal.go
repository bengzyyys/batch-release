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

var gramsPattern = regexp.MustCompile(`^(\d+)(?:\.(\d{1,3}))?$`)

// parseGramsSigned 与 parseGrams 使用同样的千分之一克定点规则，但允许
// 零与负数：读取已保存的关闭结果时，逐物料核对中的累计实投量可以为零
// （没有投料），实投减应投的差额也可以为负（欠投），真实的零与负差额
// 都是合法结果，不能当成损坏。无法解析（空串、多于三位小数、非数字）
// 或超出 int64 表示范围时返回错误。
func parseGramsSigned(s string) (gramsMilli, error) {
	s = strings.TrimSpace(s)
	negative := false
	if strings.HasPrefix(s, "-") || strings.HasPrefix(s, "+") {
		negative = s[0] == '-'
		s = s[1:]
	}
	if s == "" {
		return 0, errors.New("克数不能为空")
	}
	m := gramsPattern.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("克数 %q 不合法：最多三位小数", s)
	}
	whole, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("克数 %q 不合法", s)
	}
	var frac int64
	if m[2] != "" {
		f := m[2]
		for len(f) < 3 {
			f += "0"
		}
		frac, err = strconv.ParseInt(f, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("克数 %q 不合法", s)
		}
	}
	if whole > math.MaxInt64/1000 ||
		(whole == math.MaxInt64/1000 && frac > math.MaxInt64%1000) {
		return 0, fmt.Errorf("克数 %q 超出可表示范围", s)
	}
	milli := whole*1000 + frac
	if negative {
		milli = -milli
	}
	return gramsMilli(milli), nil
}

// parseGrams 将克数字符串精确解析为千分之一克。
// 规则：必须为正数，且最多三位小数（如 100、100.5、0.001 合法；0、-1、1.2345 非法）。
func parseGrams(s string) (gramsMilli, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("克数不能为空")
	}
	m := gramsPattern.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("克数 %q 不合法：必须为正数且最多三位小数", s)
	}
	whole, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("克数 %q 不合法", s)
	}
	var frac int64
	if m[2] != "" {
		f := m[2]
		for len(f) < 3 {
			f += "0"
		}
		frac, err = strconv.ParseInt(f, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("克数 %q 不合法", s)
		}
	}
	// 先做溢出检查，避免整数回绕把超大克数变成较小的值。
	if whole > int64(maxGramsMilli)/1000 ||
		(whole == int64(maxGramsMilli)/1000 && frac > int64(maxGramsMilli)%1000) {
		return 0, fmt.Errorf("克数 %q 超出范围：单次数量不能超过 %s 克", s, maxGramsMilli)
	}
	milli := whole*1000 + frac
	if milli <= 0 {
		return 0, fmt.Errorf("克数 %q 必须为正数", s)
	}
	return gramsMilli(milli), nil
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
