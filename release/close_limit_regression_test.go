package release

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 本文件是“已关闭批次数量核对”在数量上限附近的回归保障：重点保护读取
// 已保存关闭结果时，对数量上限附近的负差额仍能精确核对的行为。关闭只确认
// 已有投料、不要求数量吻合，因此应投量恰好达到上限（9223372036854775.807
// 克）、实投为零或只有 0.001 克、差额为接近上限的负值，都是一次合法关闭
// 留下的正常结果，不能因为差额很大或实投为零就当成损坏；反过来，保存结果
// 的应投量、实投量或差额只要与配方和实际投料相差 0.001 克，即使仍在数量
// 上限内，也必须按 ErrCorruptData 拒绝——原始投料完整不能使写错的关闭
// 结果变成合法，查询也不能先用正确计算值替换它再返回成功。负差额的绝对值
// 同样不能超过数量上限，保存成 -9223372036854775.808 克时必须明确拒绝，
// 不能发生数值回绕、截短或舍入。本文件只补充现有功能的自动化检查，不增加
// 放行判断，也不改变关闭批次的条件。
//
// 复用 feeding_limit_test.go 的 limitGrams（9223372036854775.807）与
// corrupt_close_request_result_test.go 的 corruptCloseRequestResult /
// mutatedCloseView（均针对 close-b1 请求的保存结果）。

// registerLimitRecipe 登记上限核对配方 RL/v1：M1 每份恰为数量上限
// 9223372036854775.807 克，M2 每份 1 克（普通用量，用于验证批次中的其他
// 物料仍按各自配方用量核对，其数量不参与 M1 差额的计算）。
func registerLimitRecipe(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.RegisterRecipe("recipe-rl", "RL", "v1", "上限配方", []MaterialInput{
		{MaterialNo: "M1", Grams: limitGrams},
		{MaterialNo: "M2", Grams: "1"},
	}); err != nil {
		t.Fatalf("登记 RL/v1 失败: %v", err)
	}
}

// setupLimitCloseLedger 在 dir 建立一份上限核对台账并关闭：
// RL/v1（M1 每份恰为数量上限、M2 每份 1 克），B1 计划 1 份——M1 的应投量
// 恰好等于 9223372036854775.807 克。feedMilli 为 true 时给 M1 投 0.001 克
// （请求编号 f1），否则无投料；随后用 close-b1 关闭。
// 返回首次关闭成功的结果与落盘后的台账文件内容，供各用例改坏后重写。
func setupLimitCloseLedger(t *testing.T, dir string, feedMilli bool) (*BatchView, []byte) {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	registerLimitRecipe(t, s)
	if _, err := s.CreateBatch("b1", "B1", "RL", "v1", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("start-b1", "B1"); err != nil {
		t.Fatal(err)
	}
	if feedMilli {
		if _, err := s.AddFeeding("f1", "B1", "M1", "0.001", closeFeedTime(), "张三"); err != nil {
			t.Fatal(err)
		}
	}
	first, err := s.CloseBatch("close-b1", "B1")
	if err != nil {
		t.Fatalf("应投恰达上限的执行中批次应可关闭: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	good, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	return first, good
}

// checkLimitClosedView 校验 B1 关闭后的完整视图（首次关闭的返回结果、
// 重放结果或按批次查询的结果都适用）：M1 应投恰为数量上限，实投与差额由
// wantActual/wantDiff 给出；M2 按自己的配方用量核对（应投 1、实投 0、
// 差额 -1），其数量不参与 M1 差额的计算。
func checkLimitClosedView(t *testing.T, v *BatchView, wantActual, wantDiff string) {
	t.Helper()
	if v.BatchNo != "B1" || v.RecipeNo != "RL" || v.RecipeVersion != "v1" ||
		v.RecipeName != "上限配方" || v.PlannedPortions != 1 || v.Status != StatusClosed {
		t.Fatalf("应为已关闭的 RL/v1、1 份批次 B1，得到 %+v", v)
	}
	mats := materialsMap(v)
	if len(mats) != 2 {
		t.Fatalf("应只有 M1、M2 两种物料，得到 %d 项: %+v", len(mats), v.Materials)
	}
	checkRequirement(t, mats, "M1", limitGrams, wantActual, wantDiff)
	checkRequirement(t, mats, "M2", "1", "0", "-1")
}

// 应投量恰好达到数量上限、尚无投料时，关闭结果应保留该应投量、零实投和
// -9223372036854775.807 克的差额——关闭表示确认已有投料，不要求数量吻合，
// 不能因差额很大或实投为零就把这次合法关闭的结果当成损坏。重新打开台账
// 与查询该批次都应成功，重放仍返回第一次关闭时的完整结果。
func TestCloseResultAtLimitWithZeroActual(t *testing.T) {
	dir := t.TempDir()
	first, _ := setupLimitCloseLedger(t, dir, false)
	if len(first.Feedings) != 0 {
		t.Fatalf("无投料批次的投料列表应为空，得到 %d 条: %+v", len(first.Feedings), first.Feedings)
	}
	checkLimitClosedView(t, first, "0", "-"+limitGrams)

	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("应投恰达上限、零实投、负差额的关闭结果不应判为损坏: %v", err)
	}
	defer s2.Close()
	replay, err := s2.CloseBatch("close-b1", "B1")
	if err != nil {
		t.Fatalf("重放关闭请求应成功: %v", err)
	}
	checkLimitClosedView(t, replay, "0", "-"+limitGrams)
	checkLimitClosedView(t, mustGetBatch(t, s2, "B1"), "0", "-"+limitGrams)
}

// 应投量恰达上限、只投了 0.001 克时，关闭结果的实投应为 0.001 克，差额
// 应为 -9223372036854775.806 克：不能丢掉这千分之一克，也不能把负差额
// 翻成正数。重新打开台账与查询该批次都应成功，重放结果一致。
func TestCloseResultAtLimitWithMilliActual(t *testing.T) {
	dir := t.TempDir()
	first, _ := setupLimitCloseLedger(t, dir, true)
	if len(first.Feedings) != 1 || first.Feedings[0].Seq != 1 ||
		first.Feedings[0].MaterialNo != "M1" || first.Feedings[0].Grams != "0.001" {
		t.Fatalf("应保留 0.001 克的投料记录，得到 %+v", first.Feedings)
	}
	checkLimitClosedView(t, first, "0.001", "-9223372036854775.806")

	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("实投 0.001 克、负差额接近上限的关闭结果不应判为损坏: %v", err)
	}
	defer s2.Close()
	replay, err := s2.CloseBatch("close-b1", "B1")
	if err != nil {
		t.Fatalf("重放关闭请求应成功: %v", err)
	}
	checkLimitClosedView(t, replay, "0.001", "-9223372036854775.806")
	got := mustGetBatch(t, s2, "B1")
	checkLimitClosedView(t, got, "0.001", "-9223372036854775.806")
	if len(got.Feedings) != 1 || got.Feedings[0].Grams != "0.001" {
		t.Fatalf("查询应保留 0.001 克的投料记录，得到 %+v", got.Feedings)
	}
}

// 正常写法差异不能误判为损坏：合法范围内 1 与 1.000、0 与 0.000、-1 与
// -1.000 是相同数量，不能只因末尾的零不同拒绝读取。台账应正常打开，
// 重放返回保存的写法，查询返回规范化写法。
func TestCloseResultAtLimitAcceptsFormatVariants(t *testing.T) {
	dir := t.TempDir()
	_, good := setupLimitCloseLedger(t, dir, false)

	bad := corruptCloseRequestResult(t, good, func() json.RawMessage {
		return mutatedCloseView(t, good, func(v *BatchView) {
			v.Materials[0].ActualGrams = "0.000"      // 与 0 是同一数量
			v.Materials[1].RequiredGrams = "1.000"    // 与 1 是同一数量
			v.Materials[1].ActualGrams = "0.000"      // 与 0 是同一数量
			v.Materials[1].DifferenceGrams = "-1.000" // 与 -1 是同一数量
		})
	})
	if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("末尾零的写法差异不应判为损坏: %v", err)
	}
	defer s.Close()

	// 重放取回保存的结果原文：保持 0.000、1.000、-1.000 的写法。
	replay, err := s.CloseBatch("close-b1", "B1")
	if err != nil {
		t.Fatalf("重放关闭请求应成功: %v", err)
	}
	mats := materialsMap(replay)
	if mats["M1"].ActualGrams != "0.000" || mats["M2"].RequiredGrams != "1.000" ||
		mats["M2"].ActualGrams != "0.000" || mats["M2"].DifferenceGrams != "-1.000" {
		t.Fatalf("重放应返回保存的核对项写法，得到 %+v", replay.Materials)
	}
	// 按批次查询得到系统规范化写法的视图，数量与首次关闭一致。
	checkLimitClosedView(t, mustGetBatch(t, s, "B1"), "0", "-"+limitGrams)
}

// 保存结果的应投量、实投量或差额只要与配方和实际投料相差 0.001 克，即使
// 仍在数量上限内，也必须返回 ErrCorruptData：数量相同与数量不符之间的
// 区别必须保住，原始投料完整不能使写错的关闭结果变成合法。错误信息应指出
// 关闭请求编号、批次编号及对应物料，原台账内容保持不变。
func TestOpenRejectsCloseResultOffByMilliNearLimit(t *testing.T) {
	cases := []struct {
		name      string
		feedMilli bool // 是否使用投过 0.001 克的台账
		edit      func(v *BatchView)
		material  string // 错误信息应指出的物料
	}{
		{"应投量少 0.001", false, func(v *BatchView) {
			v.Materials[0].RequiredGrams = "9223372036854775.806"
		}, "M1"},
		{"实投量多出 0.001", false, func(v *BatchView) {
			v.Materials[0].ActualGrams = "0.001"
		}, "M1"},
		{"负差额少算 0.001", false, func(v *BatchView) {
			v.Materials[0].DifferenceGrams = "-9223372036854775.806"
		}, "M1"},
		{"负差额被翻成正数", false, func(v *BatchView) {
			v.Materials[0].DifferenceGrams = limitGrams
		}, "M1"},
		{"其他物料差额差 0.001", false, func(v *BatchView) {
			v.Materials[1].DifferenceGrams = "-1.001"
		}, "M2"},
		{"实投丢掉千分之一克", true, func(v *BatchView) {
			v.Materials[0].ActualGrams = "0"
		}, "M1"},
		{"已投 0.001 时差额多算 0.001", true, func(v *BatchView) {
			v.Materials[0].DifferenceGrams = "-9223372036854775.807"
		}, "M1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			_, good := setupLimitCloseLedger(t, dir, tc.feedMilli)
			bad := corruptCloseRequestResult(t, good, func() json.RawMessage {
				return mutatedCloseView(t, good, tc.edit)
			})
			if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
				t.Fatal(err)
			}

			s, err := Open(dir)
			if !errors.Is(err, ErrCorruptData) {
				t.Fatalf("相差 0.001 克的关闭结果应返回 ErrCorruptData，得到 %v", err)
			}
			if s != nil {
				s.Close()
				t.Fatalf("损坏台账不应返回可用的 Store 对象")
			}
			msg := err.Error()
			for _, want := range []string{"close-b1", "B1", tc.material} {
				if !strings.Contains(msg, want) {
					t.Fatalf("错误信息应包含 %q，得到 %v", want, err)
				}
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

// 台账正常打开后，保存内容中的关闭结果被改坏（差额差 0.001 克，原始投料
// 保持完整）：下一次查询与重放都必须返回 ErrCorruptData——查询不能先用
// 正确计算值替换写错的保存结果再返回成功，重放也不能把损坏结果当成第一次
// 关闭的结果。被拒绝的访问不改动原台账内容。
func TestCloseResultCorruptionNearLimitNotHealedByRecompute(t *testing.T) {
	dir := t.TempDir()
	_, good := setupLimitCloseLedger(t, dir, true)

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	defer s.Close()

	// 打开后把 close-b1 保存结果的差额改坏 0.001 克：原始投料（0.001 克）
	// 与应投量都保持完整，只有核对差额与配方和实际投料不符。
	bad := corruptCloseRequestResult(t, good, func() json.RawMessage {
		return mutatedCloseView(t, good, func(v *BatchView) {
			v.Materials[0].DifferenceGrams = "-9223372036854775.807"
		})
	})
	if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
		t.Fatal(err)
	}

	// 查询该批次：不能用重算的正确差额覆盖写错的保存结果后返回成功。
	if v, err := s.GetBatch("B1"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("查询应返回 ErrCorruptData，得到 %v", err)
	} else if v != nil {
		t.Fatalf("被拒绝的查询不应返回部分台账视图: %+v", v)
	}
	// 重放关闭请求：不能把损坏的保存结果当成第一次关闭的结果返回。
	if _, err := s.CloseBatch("close-b1", "B1"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("重放损坏的关闭请求应返回 ErrCorruptData，得到 %v", err)
	}

	after, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, bad) {
		t.Fatalf("被拒绝的访问不应改动台账文件")
	}
}

// 负差额的绝对值同样不能超过数量上限：保存成 -9223372036854775.808 克
// （或更大的负值）时必须明确拒绝，不能发生数值回绕、截短或舍入后当成合法
// 结果读取。应投量超过上限的写法同理。错误信息应指出关闭请求编号、批次
// 编号及对应物料的数量问题，原台账内容保持不变。
func TestOpenRejectsCloseResultQuantityBeyondLimit(t *testing.T) {
	cases := []struct {
		name string
		edit func(v *BatchView)
	}{
		{"负差额绝对值超上限 0.001", func(v *BatchView) {
			v.Materials[0].DifferenceGrams = "-9223372036854775.808"
		}},
		{"负差额整数部分超上限", func(v *BatchView) {
			v.Materials[0].DifferenceGrams = "-9223372036854776"
		}},
		{"负差额远超 int64 不能回绕", func(v *BatchView) {
			v.Materials[0].DifferenceGrams = "-18446744073709551.615"
		}},
		{"应投量超上限 0.001", func(v *BatchView) {
			v.Materials[0].RequiredGrams = "9223372036854775.808"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			_, good := setupLimitCloseLedger(t, dir, false)
			bad := corruptCloseRequestResult(t, good, func() json.RawMessage {
				return mutatedCloseView(t, good, tc.edit)
			})
			if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
				t.Fatal(err)
			}

			s, err := Open(dir)
			if !errors.Is(err, ErrCorruptData) {
				t.Fatalf("超出数量上限的关闭结果应返回 ErrCorruptData，得到 %v", err)
			}
			if s != nil {
				s.Close()
				t.Fatalf("损坏台账不应返回可用的 Store 对象")
			}
			msg := err.Error()
			for _, want := range []string{"close-b1", "B1", "M1"} {
				if !strings.Contains(msg, want) {
					t.Fatalf("错误信息应包含 %q，得到 %v", want, err)
				}
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
