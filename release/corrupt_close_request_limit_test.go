package release

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 本文件为“读取已保存的成功关闭请求结果时的逐物料数量核对”补充边界回归
// 保障，专门保护数量上限附近的负差额：
//
// 关闭只确认已有投料、不要求数量已经吻合。某物料的应投量恰好达到上限
// 9223372036854775.807 克时，零实投与负的整额差额 -9223372036854775.807
// 克是一次合法关闭应有的结果；只投 0.001 克时，实投必须是 0.001 克、
// 差额必须是 -9223372036854775.806 克。读取台账、查询该批次与重放关闭
// 请求都必须成功并精确保留这些值，不能因为差额很大或实投为零把合法关闭
// 结果当成损坏，不能丢掉千分之一克，也不能把负差额翻成正数。
//
// 同时保护“数值相同”与“数值不符”的界限：1 与 1.000、0 与 0.000、
// -1 与 -1.000 是相同数量，不因末尾的零拒绝读取；但应投量、实投量或
// 差额只要与配方和实际投料相差 0.001 克（即使仍在数量上限内），原始投料
// 再完整也必须返回 ErrCorruptData，查询也不能先用重算的正确值替换坏结果
// 再返回成功。负差额的绝对值同样不能超过上限：保存成
// -9223372036854775.808 克等越界写法必须明确拒绝，不能发生数值回绕、
// 截短或舍入；错误需指出关闭请求编号、批次编号与对应物料，原台账不变。
//
// 本文件只补充现有行为的自动化检查：不增加任何放行判断，也不改变关闭
// 批次的条件（零实投、巨额欠投依旧可以按原规则关闭）。
//
// 复用配方 RL/v1（一份计划）：
//   - Mbig：每份恰好为上限 9223372036854775.807 克，一份时应投即上限；
//   - M2：每份 1 克，用于验证其他物料按各自配方用量独立核对，其数量不
//     参与 Mbig 差额的计算，也不被 Mbig 的巨额负差额抵消。

const (
	// negMaxGrams 是上限的负值：零实投、应投恰为上限时的合法差额。
	negMaxGrams = "-9223372036854775.807"
	// maxGramsLessMilli 是上限减 0.001 克（正值写法）。
	maxGramsLessMilli = "9223372036854775.806"
	// negMaxGramsLessMilli 是“只投 0.001 克”时的合法差额：-(上限-0.001)。
	negMaxGramsLessMilli = "-9223372036854775.806"
)

// limitCloseFeedTime 是上限关闭台账投料使用的固定时间。
func limitCloseFeedTime() time.Time {
	return time.Date(2026, 11, 15, 9, 30, 0, 0, time.UTC)
}

// buildLimitCloseLedger 在 dir 建立一份合法台账并关闭后落盘：
//   - 登记 RL/v1：Mbig 每份恰好为上限、M2 每份 1 克；
//   - 创建 B1（RL/v1、1 份），开始执行；
//   - feedMilli 为真时给 Mbig 登记唯一一条 0.001 克投料，否则不投料；
//   - 用 "close-big" 关闭。
//
// 返回落盘台账内容与首次关闭成功的结果。一份时 Mbig 应投恰好等于上限，
// 是“应投在上限、实投为零或仅 0.001 克”的边界台账。
func buildLimitCloseLedger(t *testing.T, dir string, feedMilli bool) ([]byte, *BatchView) {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	if _, err := s.RegisterRecipe("recipe-rl", "RL", "v1", "上限物料配方", []MaterialInput{
		{MaterialNo: "Mbig", Grams: limitGrams},
		{MaterialNo: "M2", Grams: "1"},
	}); err != nil {
		t.Fatalf("登记 RL/v1 失败: %v", err)
	}
	if _, err := s.CreateBatch("b-big", "B1", "RL", "v1", 1); err != nil {
		t.Fatalf("创建批次失败: %v", err)
	}
	if _, err := s.StartBatch("start-big", "B1"); err != nil {
		t.Fatalf("开始执行失败: %v", err)
	}
	if feedMilli {
		if _, err := s.AddFeeding("feed-big-milli", "B1", "Mbig", "0.001", limitCloseFeedTime(), "张三"); err != nil {
			t.Fatalf("登记 0.001 克投料失败: %v", err)
		}
	}
	first, err := s.CloseBatch("close-big", "B1")
	if err != nil {
		t.Fatalf("实投远低于应投（含零实投）也应能按原规则关闭: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("关闭台账失败: %v", err)
	}
	good, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	return good, first
}

// rewriteLimitCloseResult 从合法台账内容中取出 close-big 的保存结果，
// 经 edit 改写后重新落盘为完整台账内容（批次、配方与其他请求记录不动）。
func rewriteLimitCloseResult(t *testing.T, good []byte, edit func(*BatchView)) []byte {
	t.Helper()
	var st persistedState
	if err := json.Unmarshal(good, &st); err != nil {
		t.Fatal(err)
	}
	req := st.Requests["close-big"]
	if req == nil {
		t.Fatalf("合法台账中应存在 close-big 请求记录")
	}
	var v BatchView
	if err := json.Unmarshal(req.Result, &v); err != nil {
		t.Fatal(err)
	}
	edit(&v)
	raw, err := json.Marshal(&v)
	if err != nil {
		t.Fatal(err)
	}
	req.Result = raw
	bad, err := json.MarshalIndent(&st, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return bad
}

// closeMaterialItem 按物料编号取出关闭结果中的核对项指针。
func closeMaterialItem(t *testing.T, v *BatchView, materialNo string) *MaterialRequirement {
	t.Helper()
	for i := range v.Materials {
		if v.Materials[i].MaterialNo == materialNo {
			return &v.Materials[i]
		}
	}
	t.Fatalf("关闭结果缺少物料 %q 的核对项: %+v", materialNo, v.Materials)
	return nil
}

// checkLimitClosedView 精确核对 B1 关闭结果（首次返回、重放或查询视图）：
// Mbig 应投恒为上限；零实投时差额为负的整额上限，投 0.001 克时实投与
// 差额都要精确保留千分之一克；M2 始终按自己的每份 1 克独立核对，不被
// Mbig 的巨额差额卷入。
func checkLimitClosedView(t *testing.T, v *BatchView, feedMilli bool) {
	t.Helper()
	if v.BatchNo != "B1" || v.RecipeNo != "RL" || v.RecipeVersion != "v1" ||
		v.RecipeName != "上限物料配方" || v.PlannedPortions != 1 || v.Status != StatusClosed {
		t.Fatalf("应为已关闭的 RL/v1、1 份批次 B1，得到 %+v", v)
	}
	mats := materialsMap(v)
	if len(mats) != 2 {
		t.Fatalf("应只有 Mbig、M2 两项核对，得到 %+v", v.Materials)
	}
	if feedMilli {
		// 实投 0.001 克不能丢：实投就是 0.001，差额只比负的整额上限少欠
		// 0.001 克，仍是负数，不能翻正也不能记成零实投的 -...807。
		checkRequirement(t, mats, "Mbig", limitGrams, "0.001", negMaxGramsLessMilli)
		if len(v.Feedings) != 1 {
			t.Fatalf("应保留唯一一条 0.001 克投料，得到 %d 条", len(v.Feedings))
		}
		f := v.Feedings[0]
		if f.Seq != 1 || f.MaterialNo != "Mbig" || f.Grams != "0.001" ||
			!f.Time.Equal(limitCloseFeedTime()) || f.Registrar != "张三" {
			t.Fatalf("0.001 克投料应原样保留，得到 %+v", f)
		}
	} else {
		// 零实投与负的整额差额是合法关闭的正常结果。
		checkRequirement(t, mats, "Mbig", limitGrams, "0", negMaxGrams)
		if len(v.Feedings) != 0 {
			t.Fatalf("无投料批次不应保留投料，得到 %d 条: %+v", len(v.Feedings), v.Feedings)
		}
	}
	// 其他物料按各自配方用量核对：应投 1、实投 0、差额 -1，
	// 其数量不参与 Mbig 上限附近差额的计算，也不与之抵消。
	checkRequirement(t, mats, "M2", "1", "0", "-1")
}

// 应投恰好达到上限时，零实投与只投 0.001 克的关闭结果都必须精确保留：
// 首次关闭返回、重新打开台账、查询该批次、重放关闭请求四条路径都成功，
// 差额保持负数、千分之一克不丢。关闭不以数量吻合为条件，这里不新增任何
// 放行判断。
func TestCloseResultAtQuantityLimitPreservesExactNegativeDifference(t *testing.T) {
	cases := []struct {
		name      string
		feedMilli bool
	}{
		{"应投为上限且尚无投料", false},
		{"应投为上限且只投0.001克", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			_, first := buildLimitCloseLedger(t, dir, tc.feedMilli)
			// 首次关闭结果：零实投/巨额负差，或缺一千分之一克都如实保留。
			checkLimitClosedView(t, first, tc.feedMilli)

			// 重新打开台账：合法关闭结果不能因差额很大或实投为零被判损坏。
			s, err := Open(dir)
			if err != nil {
				t.Fatalf("读取含上限负差额的关闭结果应成功: %v", err)
			}
			t.Cleanup(func() { s.Close() })

			// 查询该批次成功，数量与首次关闭一致（系统规范化写法）。
			checkLimitClosedView(t, mustGetBatch(t, s, "B1"), tc.feedMilli)

			// 重放关闭请求取回首次结果，仍精确保留上限、实投与负差额。
			replay, err := s.CloseBatch("close-big", "B1")
			if err != nil {
				t.Fatalf("重放关闭请求应成功: %v", err)
			}
			checkLimitClosedView(t, replay, tc.feedMilli)

			// 再查询仍成功，重放不改变台账。
			checkLimitClosedView(t, mustGetBatch(t, s, "B1"), tc.feedMilli)
		})
	}
}

// 合法范围内的末尾零写法差异是同一数量：上限物料的 0 实投写成 0.000，
// 其他物料的 1/0/-1 写成 1.000/0.000/-1.000，读取台账与查询都不得拒绝；
// 上限应投量与负的整额差额按原值照读。重放保留保存时的写法，查询返回
// 系统规范化写法。
func TestOpenAcceptsTrailingZeroEquivalentsInCloseResultAtLimit(t *testing.T) {
	dir := t.TempDir()
	good, _ := buildLimitCloseLedger(t, dir, false)
	bad := rewriteLimitCloseResult(t, good, func(v *BatchView) {
		// Mbig：应投仍是上限、差额仍是负的整额上限（均已三位小数），
		// 零实投写成 0.000——与 0 是同一数量。
		mbig := closeMaterialItem(t, v, "Mbig")
		mbig.RequiredGrams = limitGrams
		mbig.ActualGrams = "0.000"
		mbig.DifferenceGrams = negMaxGrams
		// M2：1 与 1.000、0 与 0.000、-1 与 -1.000 分别相等。
		m2 := closeMaterialItem(t, v, "M2")
		m2.RequiredGrams = "1.000"
		m2.ActualGrams = "0.000"
		m2.DifferenceGrams = "-1.000"
	})
	if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("末尾零写法差异不应判为损坏: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	// 重放返回保存的原文写法，上限值与负整额差额照读，1/0/-1 的零保留。
	replay, err := s.CloseBatch("close-big", "B1")
	if err != nil {
		t.Fatalf("重放关闭请求应成功: %v", err)
	}
	rm := materialsMap(replay)
	if rm["Mbig"].RequiredGrams != limitGrams || rm["Mbig"].ActualGrams != "0.000" ||
		rm["Mbig"].DifferenceGrams != negMaxGrams {
		t.Fatalf("重放应保留 Mbig 的上限写法与 0.000 实投，得到 %+v", rm["Mbig"])
	}
	if rm["M2"].RequiredGrams != "1.000" || rm["M2"].ActualGrams != "0.000" ||
		rm["M2"].DifferenceGrams != "-1.000" {
		t.Fatalf("重放应保留 M2 的末尾零写法，得到 %+v", rm["M2"])
	}

	// 查询返回规范化数量，内容仍是同一份合法核对。
	checkLimitClosedView(t, mustGetBatch(t, s, "B1"), false)
}

// 保存结果的应投量、实投量或差额与配方、实际投料相差 0.001 克时，即使
// 偏差后的值仍在数量上限内，Open 也必须返回 ErrCorruptData：原始投料
// 完整不能使写错的关闭结果变合法。负差额被翻正、被改零同样拒绝。其他
// 物料按各自用量独立核对（只改坏 M2 时错误指向 M2，Mbig 正确也不抵消）。
// 负差额绝对值超过上限的写法必须明确拒绝，不能回绕、截短或舍入。所有
// 拒绝都要指出关闭请求编号、批次编号与对应物料，且原台账文件保持不变。
func TestOpenRejectsMismatchedCloseResultQuantitiesAtLimit(t *testing.T) {
	cases := []struct {
		name      string
		feedMilli bool
		edit      func(v *BatchView)
		material  string // 错误信息必须指出的物料
		rawValue  string // 非空时要求错误信息包含该坏值原文（防回绕/截短）
	}{
		// —— 偏差后仍在合法范围内，但与依据相差 0.001 克：必须拒绝 ——
		{
			name:      "应投量比配方少0.001克但仍在限内",
			feedMilli: true, material: "Mbig",
			edit: func(v *BatchView) { closeMaterialItem(t, v, "Mbig").RequiredGrams = maxGramsLessMilli },
		},
		{
			name:      "实投量丢掉千分之一克（0.001写成0）",
			feedMilli: true, material: "Mbig",
			edit: func(v *BatchView) { closeMaterialItem(t, v, "Mbig").ActualGrams = "0" },
		},
		{
			name:      "实投量多写0.001克",
			feedMilli: true, material: "Mbig",
			edit: func(v *BatchView) { closeMaterialItem(t, v, "Mbig").ActualGrams = "0.002" },
		},
		{
			name:      "差额被记成零实投的全欠值（少0.001克）",
			feedMilli: true, material: "Mbig",
			edit: func(v *BatchView) { closeMaterialItem(t, v, "Mbig").DifferenceGrams = negMaxGrams },
		},
		{
			name:      "差额多欠0.001克",
			feedMilli: true, material: "Mbig",
			edit: func(v *BatchView) { closeMaterialItem(t, v, "Mbig").DifferenceGrams = "-9223372036854775.805" },
		},
		{
			name:      "负差额被翻成正数",
			feedMilli: true, material: "Mbig",
			edit: func(v *BatchView) { closeMaterialItem(t, v, "Mbig").DifferenceGrams = maxGramsLessMilli },
		},
		{
			name:      "负差额被改成零",
			feedMilli: true, material: "Mbig",
			edit: func(v *BatchView) { closeMaterialItem(t, v, "Mbig").DifferenceGrams = "0" },
		},
		{
			name:      "零实投被凭空写成0.001克",
			feedMilli: false, material: "Mbig",
			edit: func(v *BatchView) { closeMaterialItem(t, v, "Mbig").ActualGrams = "0.001" },
		},
		{
			// Mbig 三项全部正确，只把其他物料 M2 的应投量写错 0.001 克：
			// 仍按 M2 自己的每份用量核对并拒绝，Mbig 的数量不参与抵消。
			name:      "其他物料应投量写错0.001克",
			feedMilli: true, material: "M2",
			edit: func(v *BatchView) { closeMaterialItem(t, v, "M2").RequiredGrams = "0.999" },
		},
		// —— 负差额绝对值超过上限：明确拒绝，不得回绕、截短、舍入 ——
		{
			name:      "负差额比上限多负0.001克",
			feedMilli: true, material: "Mbig", rawValue: "-9223372036854775.808",
			edit: func(v *BatchView) { closeMaterialItem(t, v, "Mbig").DifferenceGrams = "-9223372036854775.808" },
		},
		{
			name:      "负差额整数部分已超上限",
			feedMilli: true, material: "Mbig", rawValue: "-9223372036854776",
			edit: func(v *BatchView) { closeMaterialItem(t, v, "Mbig").DifferenceGrams = "-9223372036854776" },
		},
		{
			name:      "两倍量级负值不得回绕成合法小数量",
			feedMilli: true, material: "Mbig", rawValue: "-18446744073709551.615",
			edit: func(v *BatchView) { closeMaterialItem(t, v, "Mbig").DifferenceGrams = "-18446744073709551.615" },
		},
		{
			name:      "四位小数差额不得舍入到合法值",
			feedMilli: true, material: "Mbig", rawValue: "-9223372036854775.8079",
			edit: func(v *BatchView) { closeMaterialItem(t, v, "Mbig").DifferenceGrams = "-9223372036854775.8079" },
		},
		{
			name:      "四位小数末位为零不得截短后接受",
			feedMilli: true, material: "Mbig", rawValue: "-9223372036854775.8070",
			edit: func(v *BatchView) { closeMaterialItem(t, v, "Mbig").DifferenceGrams = "-9223372036854775.8070" },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			good, _ := buildLimitCloseLedger(t, dir, tc.feedMilli)
			bad := rewriteLimitCloseResult(t, good, tc.edit)
			if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
				t.Fatal(err)
			}

			s, err := Open(dir)
			if !errors.Is(err, ErrCorruptData) {
				t.Fatalf("关闭结果数量不符应返回 ErrCorruptData，得到 %v", err)
			}
			if s != nil {
				s.Close()
				t.Fatalf("损坏台账不应返回可用的 Store 对象")
			}
			msg := err.Error()
			for _, want := range []string{"close-big", "B1", tc.material} {
				if !strings.Contains(msg, want) {
					t.Fatalf("错误信息应包含关闭请求编号、批次编号与物料 %q，得到 %v", want, err)
				}
			}
			if tc.rawValue != "" && !strings.Contains(msg, tc.rawValue) {
				t.Fatalf("错误信息应保留坏值原文 %q（不得回绕/截短/舍入），得到 %v", tc.rawValue, err)
			}
			got, err := os.ReadFile(filepath.Join(dir, stateFileName))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, bad) {
				t.Fatalf("拒绝打开不应改动台账文件")
			}
		})
	}
}

// 台账正常打开后，已保存关闭结果的数量被改坏（与实际投料相差 0.001 克）：
// 下一次查询与重放都必须返回 ErrCorruptData，不能先用重算的正确值替换坏
// 结果再返回成功；被拒绝的访问不改写文件。恢复正确内容后查询成功，
// 0.001 克实投与负差额依旧精确保留。
func TestMismatchedCloseResultAfterOpenRejectedByQueryAndReplay(t *testing.T) {
	dir := t.TempDir()
	good, _ := buildLimitCloseLedger(t, dir, true)

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	// 打开后把 Mbig 差额从正确的 -...806 改成零实投时的 -...807（相差
	// 0.001 克），批次、配方、原始 0.001 克投料都保持完整。
	bad := rewriteLimitCloseResult(t, good, func(v *BatchView) {
		closeMaterialItem(t, v, "Mbig").DifferenceGrams = negMaxGrams
	})
	if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
		t.Fatal(err)
	}

	// 查询不能用重新计算的正确差额替换坏结果后返回成功。
	view, err := s.GetBatch("B1")
	if !errors.Is(err, ErrCorruptData) {
		t.Fatalf("关闭结果数量损坏后查询应返回 ErrCorruptData，得到 %v", err)
	}
	if view != nil {
		t.Fatalf("被拒绝的查询不应返回部分视图: %+v", view)
	}
	msg := err.Error()
	for _, want := range []string{"close-big", "B1", "Mbig"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("错误信息应包含关闭请求编号、批次编号与物料，得到 %v", err)
		}
	}

	// 重放同样不能返回损坏的保存结果，也不能重做关闭后冒充成功。
	if _, err := s.CloseBatch("close-big", "B1"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("重放损坏的关闭请求应返回 ErrCorruptData，得到 %v", err)
	}

	// 被拒绝的访问不改写台账文件。
	after, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, bad) {
		t.Fatalf("被拒绝的访问不应改动台账文件")
	}

	// 恢复正确关闭结果后查询成功，0.001 克实投与 -...806 差额精确可读。
	if err := os.WriteFile(filepath.Join(dir, stateFileName), good, 0o644); err != nil {
		t.Fatal(err)
	}
	checkLimitClosedView(t, mustGetBatch(t, s, "B1"), true)
}
