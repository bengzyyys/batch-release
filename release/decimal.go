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
