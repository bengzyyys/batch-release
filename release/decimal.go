package release

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// gramsMilli 以千分之一克为单位定点存储克数，避免浮点误差。
type gramsMilli int64

// maxGramsMilli 是克数上限：9223372036854775.807 克，
// 以千分之一克计恰好是 int64 最大值。
// 同一批次内每种物料的单次投料量与累计实投量都不得超过它（恰好相等允许）。
const maxGramsMilli gramsMilli = 1<<63 - 1

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
	// 检查 whole*1000 + frac 是否超过上限，避免整数回绕把大数变成较小的合法值。
	const maxWhole = int64(maxGramsMilli) / 1000
	if whole > maxWhole || (whole == maxWhole && frac > int64(maxGramsMilli)%1000) {
		return 0, fmt.Errorf("克数 %q 超出范围：单次投料不得超过 %s 克", s, maxGramsMilli)
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

// multiplyPortions 将克数与份数相乘，检查溢出。
func multiplyPortions(g gramsMilli, portions int) (gramsMilli, error) {
	if portions <= 0 {
		return 0, errors.New("份数必须为正整数")
	}
	v := int64(g)
	if v > (1<<63-1)/int64(portions) {
		return 0, fmt.Errorf("数量过大：%s 克 × %d 份溢出", g, portions)
	}
	return gramsMilli(v * int64(portions)), nil
}
