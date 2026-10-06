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

// 本文件是“已保存的成功草稿调整请求的返回结果必须对应原提交内容第一次调整
// 成功时的内容”的回归保障：草稿调整成功后，按原请求编号、原内容再次提交
// UpdateDraftBatch，只能取回第一次调整成功时的批次结果（结果采用的配方版本
// 与份数、草稿状态、空投料、实投为零、差额为应投量的负值）。台账里保存的
// 调整结果即使变成 null、空对象，或被改成另一批次同一次调整的结果、被改掉
// 配方绑定/份数/状态，或混入了批次后来的再次调整、投料与关闭现状，也不能
// 被当作成功结果返回——打开台账及之后的每次查询/写入重载，都必须核对保存
// 结果与原提交内容（原请求对应的批次必须仍然存在，结果采用的配方版本必须
// 仍然登记）一致；不一致按 ErrCorruptData 拒绝整份台账，不删除请求、不补
// 造批次，也不用批次当前计划覆盖损坏结果。

// setupUpdateDraftRequestLedger 在 dir 建立一份正常台账：
//   - B1（已关闭）：create-b1 用 R1/v2、5 份创建；u-first 把它调整为
//     R1/v1、8 份（本文件主要的被核对结果：M1=0.125×8=1、M2=0.001×8=0.008）；
//     u-second 又调整为 R1/v2、3 份，随后开始执行、登记 f1（M1）、f2（M4）
//     并关闭——u-first 的保存结果必须仍是第一次调整时的草稿内容，不能混入
//     后来的调整、投料与关闭现状；
//   - B2（草稿）：create-b2 用 R1/v1、10 份创建；u-keep-recipe 以“配方
//     留空、份数 8”调整（结果为 R1/v1、8 份，与 u-first 仅批次编号不同，
//     用于验证另一批次的调整结果不能顶替）；u-keep-portions 以“R1/v2、
//     份数传 0”调整（结果为 R1/v2、8 份）；u-nop 两者都留空（结果为
//     R1/v2、8 份）——后三条用于锁定沿用字段的核对语义，B2 也作为“本次
//     只访问另一条正常记录”场景中的正常批次；
//
// 返回落盘后的台账文件内容，供各用例改坏后重写。
func setupUpdateDraftRequestLedger(t *testing.T, dir string) []byte {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	registerCreateReplayRecipes(t, s) // R1/v1：M1=0.125、M2=0.001；R1/v2：M1=250、M4=2
	if _, err := s.CreateBatch("create-b1", "B1", "R1", "v2", 5); err != nil {
		t.Fatal(err)
	}
	// 被核对的第一次调整：明确指定 R1/v1、8 份。
	if _, err := s.UpdateDraftBatch("u-first", "B1", "R1", "v1", 8); err != nil {
		t.Fatal(err)
	}
	// 后来又合法调整为 R1/v2、3 份。
	if _, err := s.UpdateDraftBatch("u-second", "B1", "R1", "v2", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("start-b1", "B1"); err != nil {
		t.Fatal(err)
	}
	base := replayFeedTime()
	if _, err := s.AddFeeding("f1", "B1", "M1", "100.5", base, "张三"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddFeeding("f2", "B1", "M4", "2", base.Add(time.Hour), "李四"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseBatch("close-b1", "B1"); err != nil {
		t.Fatal(err)
	}

	// 另一批次 B2：其 u-keep-recipe 结果与 u-first 的计划完全相同，仅批次
	// 编号不同；B2 最终停在 R1/v2、8 份的草稿。
	if _, err := s.CreateBatch("create-b2", "B2", "R1", "v1", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateDraftBatch("u-keep-recipe", "B2", "", "", 8); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateDraftBatch("u-keep-portions", "B2", "R1", "v2", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateDraftBatch("u-nop", "B2", "", "", 0); err != nil {
		t.Fatal(err)
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	good, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	return good
}

// updateResultRaw 取出正常台账中 reqNo 请求的保存结果原文。
func updateResultRaw(t *testing.T, good []byte, reqNo string) json.RawMessage {
	t.Helper()
	var st persistedState
	if err := json.Unmarshal(good, &st); err != nil {
		t.Fatal(err)
	}
	req := st.Requests[reqNo]
	if req == nil {
		t.Fatalf("正常台账中应存在 %s 请求记录", reqNo)
	}
	return req.Result
}

// mutatedUpdateView 读出指定调整请求的保存结果，按 edit 修改后重新序列化。
func mutatedUpdateView(t *testing.T, good []byte, reqNo string, edit func(*BatchView)) json.RawMessage {
	t.Helper()
	var v BatchView
	if err := json.Unmarshal(updateResultRaw(t, good, reqNo), &v); err != nil {
		t.Fatal(err)
	}
	edit(&v)
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// corruptUpdateRequestResult 把正常台账内容中 reqNo 请求的保存结果替换为
// mutate 给出的值（mutate 返回 nil 表示删除该字段），返回改写后的文件内容。
func corruptUpdateRequestResult(t *testing.T, good []byte, reqNo string, mutate func() json.RawMessage) []byte {
	t.Helper()
	var st persistedState
	if err := json.Unmarshal(good, &st); err != nil {
		t.Fatal(err)
	}
	req := st.Requests[reqNo]
	if req == nil {
		t.Fatalf("正常台账中应存在 %s 请求记录", reqNo)
	}
	req.Result = mutate()
	data, err := json.MarshalIndent(&st, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// checkUFirstAdjustView 校验 u-first 第一次调整成功时的完整结果：
// B1、R1/v1（配方初版）、8 份、草稿、无投料；M1=0.125×8=1、
// M2=0.001×8=0.008，实投为零、差额为应投量的负值，不含 v2 独有的 M4。
func checkUFirstAdjustView(t *testing.T, v *BatchView) {
	t.Helper()
	if v.BatchNo != "B1" {
		t.Fatalf("批次编号应为 B1，得到 %q", v.BatchNo)
	}
	if v.RecipeNo != "R1" || v.RecipeVersion != "v1" || v.RecipeName != "配方初版" {
		t.Fatalf("应指向第一次调整的 R1/v1（配方初版），得到 %q %q %q",
			v.RecipeNo, v.RecipeVersion, v.RecipeName)
	}
	if v.PlannedPortions != 8 {
		t.Fatalf("计划份数应为第一次调整的 8，得到 %d", v.PlannedPortions)
	}
	if v.Status != StatusDraft {
		t.Fatalf("第一次调整的结果应为草稿，得到 %s", v.Status)
	}
	if len(v.Feedings) != 0 {
		t.Fatalf("第一次调整的结果不应有投料，得到 %d 条", len(v.Feedings))
	}
	if len(v.Materials) != 2 {
		t.Fatalf("第一次调整的结果只应有 M1、M2 两种物料，得到 %d 项: %+v",
			len(v.Materials), v.Materials)
	}
	if v.Materials[0].MaterialNo != "M1" || v.Materials[1].MaterialNo != "M2" {
		t.Fatalf("物料顺序应与 R1/v1 一致（M1、M2），得到 %+v", v.Materials)
	}
	mats := materialsMap(v)
	checkRequirement(t, mats, "M1", "1", "0", "-1")         // 0.125 × 8
	checkRequirement(t, mats, "M2", "0.008", "0", "-0.008") // 0.001 × 8
	if _, leftover := mats["M4"]; leftover {
		t.Fatalf("第一次调整的结果不应包含后来版本的物料 M4: %+v", mats["M4"])
	}
}

// 已保存的成功草稿调整请求的结果缺失、为 null、为空对象、无法读成批次结果，
// 或与原提交内容第一次调整成功时的内容不一致（批次编号、配方绑定、计划份数、
// 状态、投料列表、逐物料应投量/实投量/差额有任何差异，含被另一批次的调整
// 结果顶替、混入后来的调整、投料或关闭现状）时，Open 必须返回
// ErrCorruptData：错误信息指出问题请求编号及能确定的批次编号，不返回可用的
// 台账对象，也不改写原文件。
func TestOpenRejectsCorruptUpdateDraftRequestResult(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(good []byte) json.RawMessage
	}{
		{"结果缺失", func(good []byte) json.RawMessage { return nil }},
		{"结果为 null", func(good []byte) json.RawMessage { return json.RawMessage("null") }},
		{"结果为空对象", func(good []byte) json.RawMessage { return json.RawMessage(`{}`) }},
		{"结果不是批次结果对象", func(good []byte) json.RawMessage {
			return json.RawMessage(`"not-an-object"`)
		}},
		{"结果被替换成另一批次的调整结果", func(good []byte) json.RawMessage {
			// B2 的 u-keep-recipe 同为 R1/v1、8 份，但那是另一批次的结果。
			return updateResultRaw(t, good, "u-keep-recipe")
		}},
		{"结果批次编号被改成另一批次", func(good []byte) json.RawMessage {
			return mutatedUpdateView(t, good, "u-first", func(v *BatchView) { v.BatchNo = "B2" })
		}},
		{"结果配方编号被改", func(good []byte) json.RawMessage {
			return mutatedUpdateView(t, good, "u-first", func(v *BatchView) { v.RecipeNo = "RX" })
		}},
		{"结果沿用后来调整的版本", func(good []byte) json.RawMessage {
			return mutatedUpdateView(t, good, "u-first", func(v *BatchView) { v.RecipeVersion = "v2" })
		}},
		{"结果采用未登记的版本号", func(good []byte) json.RawMessage {
			return mutatedUpdateView(t, good, "u-first", func(v *BatchView) { v.RecipeVersion = "v9" })
		}},
		{"结果配方名称被改", func(good []byte) json.RawMessage {
			return mutatedUpdateView(t, good, "u-first", func(v *BatchView) { v.RecipeName = "被改的名称" })
		}},
		{"结果沿用后来调整的份数", func(good []byte) json.RawMessage {
			return mutatedUpdateView(t, good, "u-first", func(v *BatchView) { v.PlannedPortions = 3 })
		}},
		{"结果份数为零", func(good []byte) json.RawMessage {
			return mutatedUpdateView(t, good, "u-first", func(v *BatchView) { v.PlannedPortions = 0 })
		}},
		{"结果状态被改成开始执行后的现状", func(good []byte) json.RawMessage {
			return mutatedUpdateView(t, good, "u-first", func(v *BatchView) { v.Status = StatusExecuting })
		}},
		{"结果状态被改成关闭后的现状", func(good []byte) json.RawMessage {
			return mutatedUpdateView(t, good, "u-first", func(v *BatchView) { v.Status = StatusClosed })
		}},
		{"结果混入了后来追加的投料", func(good []byte) json.RawMessage {
			return mutatedUpdateView(t, good, "u-first", func(v *BatchView) {
				base := replayFeedTime()
				v.Feedings = []FeedingView{
					{Seq: 1, MaterialNo: "M1", Grams: "100.5", Time: base, Registrar: "张三"},
					{Seq: 2, MaterialNo: "M4", Grams: "2", Time: base.Add(time.Hour), Registrar: "李四"},
				}
			})
		}},
		{"物料核对项少一项", func(good []byte) json.RawMessage {
			return mutatedUpdateView(t, good, "u-first", func(v *BatchView) {
				v.Materials = v.Materials[:1]
			})
		}},
		{"物料核对项混入后来版本的物料", func(good []byte) json.RawMessage {
			return mutatedUpdateView(t, good, "u-first", func(v *BatchView) {
				v.Materials = append(v.Materials, MaterialRequirement{
					MaterialNo: "M4", RequiredGrams: "6", ActualGrams: "0", DifferenceGrams: "-6",
				})
			})
		}},
		{"物料核对顺序被调换", func(good []byte) json.RawMessage {
			return mutatedUpdateView(t, good, "u-first", func(v *BatchView) {
				v.Materials[0], v.Materials[1] = v.Materials[1], v.Materials[0]
			})
		}},
		{"应投量被改成当前计划的值", func(good []byte) json.RawMessage {
			return mutatedUpdateView(t, good, "u-first", func(v *BatchView) {
				v.Materials[0].RequiredGrams = "750" // R1/v2、3 份时 M1 的应投量
			})
		}},
		{"应投量写法无法解析", func(good []byte) json.RawMessage {
			return mutatedUpdateView(t, good, "u-first", func(v *BatchView) {
				v.Materials[0].RequiredGrams = "abc"
			})
		}},
		{"实投量被改成非零", func(good []byte) json.RawMessage {
			return mutatedUpdateView(t, good, "u-first", func(v *BatchView) {
				v.Materials[0].ActualGrams = "100.5"
			})
		}},
		{"差额被改坏", func(good []byte) json.RawMessage {
			return mutatedUpdateView(t, good, "u-first", func(v *BatchView) {
				v.Materials[0].DifferenceGrams = "0"
			})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			good := setupUpdateDraftRequestLedger(t, dir)
			bad := corruptUpdateRequestResult(t, good, "u-first", func() json.RawMessage {
				return tc.mutate(good)
			})
			if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
				t.Fatal(err)
			}

			s, err := Open(dir)
			if !errors.Is(err, ErrCorruptData) {
				t.Fatalf("保存的调整结果损坏应返回 ErrCorruptData，得到 %v", err)
			}
			if s != nil {
				s.Close()
				t.Fatalf("损坏台账不应返回可用的 Store 对象")
			}
			msg := err.Error()
			for _, want := range []string{"u-first", "B1"} {
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

// 原提交内容本身被改坏（无法解析，或批次编号为空、只给配方编号/版本号、
// 份数为负等不符合调整要求的内容）时，不可能对应一次成功的调整：Open 必须
// 返回 ErrCorruptData，错误信息指出问题请求编号。
func TestOpenRejectsCorruptUpdateDraftRequestPayload(t *testing.T) {
	cases := []struct {
		name    string
		payload string
	}{
		{"提交内容无法解析", `not-json`},
		{"提交内容批次编号为空", `{"BatchNo":"","RecipeNo":"R1","Version":"v1","Portions":8}`},
		{"只给配方编号不给版本号", `{"BatchNo":"B1","RecipeNo":"R1","Version":"","Portions":8}`},
		{"只给版本号不给配方编号", `{"BatchNo":"B1","RecipeNo":"","Version":"v1","Portions":8}`},
		{"提交内容份数为负", `{"BatchNo":"B1","RecipeNo":"R1","Version":"v1","Portions":-2}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			good := setupUpdateDraftRequestLedger(t, dir)
			var st persistedState
			if err := json.Unmarshal(good, &st); err != nil {
				t.Fatal(err)
			}
			st.Requests["u-first"].Payload = tc.payload
			bad, err := json.MarshalIndent(&st, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
				t.Fatal(err)
			}

			s, err := Open(dir)
			if !errors.Is(err, ErrCorruptData) {
				t.Fatalf("提交内容损坏应返回 ErrCorruptData，得到 %v", err)
			}
			if s != nil {
				s.Close()
				t.Fatalf("损坏台账不应返回可用的 Store 对象")
			}
			if !strings.Contains(err.Error(), "u-first") {
				t.Fatalf("错误信息应指出问题请求编号 u-first，得到 %v", err)
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

// 原调整请求对应的批次不存在时，保存的调整结果不能单独作为调整成功的依据：
// 即使结果本身内容完整，也按损坏处理，错误信息指出请求编号与批次编号。
func TestOpenRejectsUpdateDraftRequestResultWhenBatchMissing(t *testing.T) {
	dir := t.TempDir()
	good := setupUpdateDraftRequestLedger(t, dir)
	var st persistedState
	if err := json.Unmarshal(good, &st); err != nil {
		t.Fatal(err)
	}
	// 删除 B1，并移除依附于它的其他请求记录（创建、第二次调整、开始、
	// 投料、关闭），唯独保留 u-first，让“批次缺失”成为唯一损坏点。
	kept := st.Batches[:0]
	for _, b := range st.Batches {
		if b.BatchNo != "B1" {
			kept = append(kept, b)
		}
	}
	st.Batches = kept
	for _, reqNo := range []string{"create-b1", "u-second", "start-b1", "f1", "f2", "close-b1"} {
		delete(st.Requests, reqNo)
	}
	bad, err := json.MarshalIndent(&st, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	if !errors.Is(err, ErrCorruptData) {
		t.Fatalf("原批次不存在但保留调整结果应返回 ErrCorruptData，得到 %v", err)
	}
	if s != nil {
		s.Close()
		t.Fatalf("损坏台账不应返回可用的 Store 对象")
	}
	msg := err.Error()
	for _, want := range []string{"u-first", "B1"} {
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
}

// 保存结果采用的配方版本未登记时，结果不能单独作为调整成功的依据：同编号
// 配方的其他版本（R1/v2）即使仍被 B1 当前计划使用，也不能顶替 u-first
// 采用的 R1/v1。
func TestOpenRejectsUpdateDraftRequestResultWhenAdoptedVersionMissing(t *testing.T) {
	dir := t.TempDir()
	good := setupUpdateDraftRequestLedger(t, dir)
	var st persistedState
	if err := json.Unmarshal(good, &st); err != nil {
		t.Fatal(err)
	}
	// 删除 R1/v1 及其登记请求；同时删除创建时采用 v1 的 B2 与依附请求，
	// 让 u-first 的“采用版本缺失”成为唯一损坏点（B1 现状为 v2，不受影响）。
	kept := st.Recipes[:0]
	for _, r := range st.Recipes {
		if !(r.RecipeNo == "R1" && r.Version == "v1") {
			kept = append(kept, r)
		}
	}
	st.Recipes = kept
	batches := st.Batches[:0]
	for _, b := range st.Batches {
		if b.BatchNo != "B2" {
			batches = append(batches, b)
		}
	}
	st.Batches = batches
	for _, reqNo := range []string{
		"c-recipe-v1", "create-b2", "u-keep-recipe", "u-keep-portions", "u-nop",
	} {
		delete(st.Requests, reqNo)
	}
	bad, err := json.MarshalIndent(&st, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	if !errors.Is(err, ErrCorruptData) {
		t.Fatalf("采用版本不存在但保留调整结果应返回 ErrCorruptData，得到 %v", err)
	}
	if s != nil {
		s.Close()
		t.Fatalf("损坏台账不应返回可用的 Store 对象")
	}
	msg := err.Error()
	for _, want := range []string{"u-first", "B1"} {
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
}

// 沿用字段的保存结果同样必须完整自洽：即使原请求把配方留空或份数传零，
// 结果仍须是草稿、无投料、份数为正、采用已登记版本，数量核对按结果采用
// 的版本与份数完整列出。任何一项被改坏都按损坏拒绝。
func TestOpenRejectsCorruptInheritedUpdateDraftResult(t *testing.T) {
	cases := []struct {
		name   string
		reqNo  string
		mutate func(*BatchView)
	}{
		// u-keep-recipe 的原请求只明确份数 8、配方留空；份数改成非正即损坏。
		{"沿用配方的结果份数被改成零", "u-keep-recipe", func(v *BatchView) { v.PlannedPortions = 0 }},
		{"沿用配方的结果状态被改成关闭", "u-keep-recipe", func(v *BatchView) { v.Status = StatusClosed }},
		{"沿用配方的结果混入投料", "u-keep-recipe", func(v *BatchView) {
			v.Feedings = []FeedingView{{Seq: 1, MaterialNo: "M1", Grams: "1", Registrar: "张三"}}
		}},
		{"沿用配方的结果名称被改", "u-keep-recipe", func(v *BatchView) { v.RecipeName = "假名" }},
		{"沿用配方的结果数量与采用版本份数不符", "u-keep-recipe", func(v *BatchView) {
			v.Materials[0].RequiredGrams = "2" // 份数 8 时 M1 应为 1
		}},
		// u-keep-portions 的原请求明确 R1/v2、份数留空；份数被改成非正即损坏。
		{"沿用份数的结果份数被改成负数", "u-keep-portions", func(v *BatchView) { v.PlannedPortions = -1 }},
		{"沿用份数的结果物料少一项", "u-keep-portions", func(v *BatchView) {
			v.Materials = v.Materials[:1]
		}},
		// u-nop 的配方与份数全部沿用：任何一项不完整都不能接受。
		{"全部沿用的结果实投量被改成非零", "u-nop", func(v *BatchView) {
			v.Materials[0].ActualGrams = "3"
		}},
		{"全部沿用的结果采用版本被改成未登记版本", "u-nop", func(v *BatchView) { v.RecipeVersion = "v9" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			good := setupUpdateDraftRequestLedger(t, dir)
			bad := corruptUpdateRequestResult(t, good, tc.reqNo, func() json.RawMessage {
				return mutatedUpdateView(t, good, tc.reqNo, tc.mutate)
			})
			if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
				t.Fatal(err)
			}

			s, err := Open(dir)
			if !errors.Is(err, ErrCorruptData) {
				t.Fatalf("沿用字段的调整结果损坏应返回 ErrCorruptData，得到 %v", err)
			}
			if s != nil {
				s.Close()
				t.Fatalf("损坏台账不应返回可用的 Store 对象")
			}
			if !strings.Contains(err.Error(), tc.reqNo) {
				t.Fatalf("错误信息应指出问题请求编号 %q，得到 %v", tc.reqNo, err)
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

// 沿用字段不与批次当前计划比对：原请求把配方或份数留空时，保存结果只要
// 对“结果实际采用的版本与份数”完整自洽（版本已登记、名称一致、草稿、无
// 投料、数量核对完整）即为合法——即使该值与批次当前计划不同。重放取回的
// 就是保存结果采用的值。
func TestOpenAcceptsInheritedResultDifferingFromCurrentPlan(t *testing.T) {
	t.Run("全部沿用的结果采用另一合法版本与份数", func(t *testing.T) {
		dir := t.TempDir()
		good := setupUpdateDraftRequestLedger(t, dir)
		// u-nop 原请求两者都留空；把结果改成完整自洽的 R1/v1、8 份草稿
		// （B2 当前计划是 R1/v2、8 份）。
		bad := corruptUpdateRequestResult(t, good, "u-nop", func() json.RawMessage {
			return json.RawMessage(mustMarshalAdjustView(t, BatchView{
				BatchNo:         "B2",
				RecipeNo:        "R1",
				RecipeVersion:   "v1",
				RecipeName:      "配方初版",
				PlannedPortions: 8,
				Status:          StatusDraft,
				Feedings:        []FeedingView{},
				Materials: []MaterialRequirement{
					{MaterialNo: "M1", RequiredGrams: "1", ActualGrams: "0", DifferenceGrams: "-1"},
					{MaterialNo: "M2", RequiredGrams: "0.008", ActualGrams: "0", DifferenceGrams: "-0.008"},
				},
			}))
		})
		if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
			t.Fatal(err)
		}
		s, err := Open(dir)
		if err != nil {
			t.Fatalf("沿用字段与当前计划不同但结果自洽不应判损坏: %v", err)
		}
		defer s.Close()
		replay, err := s.UpdateDraftBatch("u-nop", "B2", "", "", 0)
		if err != nil {
			t.Fatalf("重放全部沿用的请求应成功: %v", err)
		}
		if replay.RecipeVersion != "v1" || replay.PlannedPortions != 8 || replay.Status != StatusDraft {
			t.Fatalf("重放应取回保存结果采用的 R1/v1、8 份，得到 %+v", replay)
		}
		mats := materialsMap(replay)
		checkRequirement(t, mats, "M1", "1", "0", "-1")
		checkRequirement(t, mats, "M2", "0.008", "0", "-0.008")
		// 查询仍显示 B2 当前计划 R1/v2、8 份，不被历史结果回退。
		cur := mustGetBatch(t, s, "B2")
		if cur.RecipeVersion != "v2" || cur.PlannedPortions != 8 {
			t.Fatalf("查询应显示 B2 当前计划 R1/v2、8 份，得到 %+v", cur)
		}
	})

	t.Run("沿用份数的结果采用另一合法份数且数量自洽", func(t *testing.T) {
		dir := t.TempDir()
		good := setupUpdateDraftRequestLedger(t, dir)
		// u-keep-portions 原请求明确 R1/v2、份数传 0；把结果改成完整自洽的
		// R1/v2、4 份（份数是沿用项，不与原提交或当前 8 份比对）。
		bad := corruptUpdateRequestResult(t, good, "u-keep-portions", func() json.RawMessage {
			return json.RawMessage(mustMarshalAdjustView(t, BatchView{
				BatchNo:         "B2",
				RecipeNo:        "R1",
				RecipeVersion:   "v2",
				RecipeName:      "配方改版",
				PlannedPortions: 4,
				Status:          StatusDraft,
				Feedings:        []FeedingView{},
				Materials: []MaterialRequirement{
					{MaterialNo: "M1", RequiredGrams: "1000", ActualGrams: "0", DifferenceGrams: "-1000"},
					{MaterialNo: "M4", RequiredGrams: "8", ActualGrams: "0", DifferenceGrams: "-8"},
				},
			}))
		})
		if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
			t.Fatal(err)
		}
		s, err := Open(dir)
		if err != nil {
			t.Fatalf("沿用份数与当前计划不同但结果自洽不应判损坏: %v", err)
		}
		defer s.Close()
		replay, err := s.UpdateDraftBatch("u-keep-portions", "B2", "R1", "v2", 0)
		if err != nil {
			t.Fatalf("重放沿用份数的请求应成功: %v", err)
		}
		if replay.PlannedPortions != 4 {
			t.Fatalf("重放应取回保存结果采用的 4 份，得到 %d", replay.PlannedPortions)
		}
		mats := materialsMap(replay)
		checkRequirement(t, mats, "M1", "1000", "0", "-1000")
		checkRequirement(t, mats, "M4", "8", "0", "-8")
	})
}

func mustMarshalAdjustView(t *testing.T, v BatchView) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// 正常写法差异不能误判为损坏：保存结果的应投量写成 1.000、实投量写成
// 0.000、差额写成 -1.000，与第一次调整时的数量是同一数值；合法的零实投与
// 负差额不是损坏。台账应正常打开，重放仍返回保存的写法。
func TestOpenAcceptsUpdateDraftResultFormatVariants(t *testing.T) {
	dir := t.TempDir()
	good := setupUpdateDraftRequestLedger(t, dir)

	bad := corruptUpdateRequestResult(t, good, "u-first", func() json.RawMessage {
		return mutatedUpdateView(t, good, "u-first", func(v *BatchView) {
			v.Materials[0].RequiredGrams = "1.000" // 与 1 是同一数量
			v.Materials[0].ActualGrams = "0.000"   // 与 0 是同一数量
			v.Materials[0].DifferenceGrams = "-1.000"
			v.Materials[1].RequiredGrams = "0.008" // 保持原值
		})
	})
	if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("写法差异不应判为损坏: %v", err)
	}
	defer s.Close()

	replay, err := s.UpdateDraftBatch("u-first", "B1", "R1", "v1", 8)
	if err != nil {
		t.Fatalf("重放调整请求应成功: %v", err)
	}
	if replay.Materials[0].RequiredGrams != "1.000" ||
		replay.Materials[0].ActualGrams != "0.000" ||
		replay.Materials[0].DifferenceGrams != "-1.000" {
		t.Fatalf("重放应返回保存的核对项写法，得到 %+v", replay.Materials[0])
	}
}

// 批次后来的再次调整、开始执行、投料与关闭都属于批次现状，不能混入第一次
// 调整的结果，也不能成为拒绝合法旧结果的理由：台账中 B1 已再次调整、开始
// 执行、追加投料并关闭，u-first 保存的仍是第一次调整时的草稿结果，打开与
// 重放都必须正常；重复提交原调整请求继续返回当次结果，查询继续显示当前
// 批次，两者互不覆盖。重新打开台账后依然成立。
func TestUpdateDraftResultStaysFirstAdjustAfterLaterLifecycle(t *testing.T) {
	dir := t.TempDir()
	setupUpdateDraftRequestLedger(t, dir)

	check := func(t *testing.T, s *Store) {
		t.Helper()
		replay, err := s.UpdateDraftBatch("u-first", "B1", "R1", "v1", 8)
		if err != nil {
			t.Fatalf("重放第一次调整请求应成功: %v", err)
		}
		checkUFirstAdjustView(t, replay)
		// 查询继续显示当前批次（已关闭、R1/v2、3 份、保留两条投料），
		// 不被第一次调整的结果回退。
		cur, err := s.GetBatch("B1")
		if err != nil {
			t.Fatalf("查询当前批次失败: %v", err)
		}
		if cur.Status != StatusClosed || cur.RecipeVersion != "v2" || cur.PlannedPortions != 3 {
			t.Fatalf("当前批次应为已关闭的 R1/v2、3 份，得到 %+v", cur)
		}
		if len(cur.Feedings) != 2 {
			t.Fatalf("当前批次应保留 2 条投料，得到 %d 条", len(cur.Feedings))
		}
		mats := materialsMap(cur)
		checkRequirement(t, mats, "M1", "750", "100.5", "-649.5") // 250×3，实投 100.5
		checkRequirement(t, mats, "M4", "6", "2", "-4")
	}

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("已再次调整并关闭的批次不应让早先草稿调整结果被判损坏: %v", err)
	}
	check(t, s)
	if err := s.Close(); err != nil {
		t.Fatalf("关闭台账失败: %v", err)
	}

	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("重新打开台账失败: %v", err)
	}
	defer s2.Close()
	check(t, s2)
}

// 台账正常打开后，保存内容中的调整请求结果被改坏：下一次查询或写入必须
// 返回 ErrCorruptData——即使本次访问的是另一个正常批次 B2，也不能忽略损坏
// 的 u-first；被拒绝的重放不得返回损坏的保存结果，被拒绝的写入不留业务
// 变化、不占用请求编号，原台账内容不变。恢复后原成功请求仍可幂等重放，
// 被拒绝过的请求编号可以正常使用。
func TestUpdateDraftRequestResultCorruptionAfterOpenDetectedOnNextAccess(t *testing.T) {
	dir := t.TempDir()
	good := setupUpdateDraftRequestLedger(t, dir)

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// 打开后把 u-first 的保存结果改坏为空对象（其余记录保持完整）。
	bad := corruptUpdateRequestResult(t, good, "u-first", func() json.RawMessage {
		return json.RawMessage(`{}`)
	})
	if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
		t.Fatal(err)
	}

	// 查询损坏批次 B1 与正常批次 B2 都必须失败，不能沿用此前读到的内容。
	for _, batchNo := range []string{"B1", "B2"} {
		if v, err := s.GetBatch(batchNo); !errors.Is(err, ErrCorruptData) {
			t.Fatalf("查询批次 %q 应返回 ErrCorruptData，得到 %v", batchNo, err)
		} else if v != nil {
			t.Fatalf("被拒绝的查询不应返回部分台账视图: %+v", v)
		}
	}
	// 用原编号、原内容重放损坏的调整请求：不得把损坏的保存结果当成成功
	// 结果返回，必须报损坏。
	if _, err := s.UpdateDraftBatch("u-first", "B1", "R1", "v1", 8); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("重放损坏的调整请求应返回 ErrCorruptData，得到 %v", err)
	}
	// 对正常批次 B2 的新写入同样不能绕过（B2 是草稿，开始执行本应成功）。
	if _, err := s.StartBatch("start-b2", "B2"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上开始正常批次应返回 ErrCorruptData，得到 %v", err)
	}

	// 被拒绝的访问不改动文件、不占用请求编号。
	after, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, bad) {
		t.Fatalf("被拒绝的访问不应改动台账文件")
	}
	if bytes.Contains(after, []byte("start-b2")) {
		t.Fatalf("被拒绝的写入不应留下请求记录")
	}

	// 恢复后：原成功请求仍取回第一次调整时的结果，被拒绝过的请求编号
	// 可以正常使用。
	if err := os.WriteFile(filepath.Join(dir, stateFileName), good, 0o644); err != nil {
		t.Fatal(err)
	}
	replay, err := s.UpdateDraftBatch("u-first", "B1", "R1", "v1", 8)
	if err != nil {
		t.Fatalf("恢复后重放原成功请求失败: %v", err)
	}
	checkUFirstAdjustView(t, replay)
	started, err := s.StartBatch("start-b2", "B2")
	if err != nil {
		t.Fatalf("被拒绝过的请求编号应仍可合法提交: %v", err)
	}
	if started.Status != StatusExecuting || started.BatchNo != "B2" {
		t.Fatalf("B2 应被正常开始执行，得到 %+v", started)
	}
}

// 任何一条成功草稿调整请求损坏都应拒绝整份台账：改坏 u-second 的结果后，
// 即使调用方只查询正常的 B2，Open 也必须返回 ErrCorruptData。
func TestAnyCorruptUpdateDraftRequestRejectsWholeLedger(t *testing.T) {
	dir := t.TempDir()
	good := setupUpdateDraftRequestLedger(t, dir)
	bad := corruptUpdateRequestResult(t, good, "u-second", func() json.RawMessage {
		return json.RawMessage(`{}`)
	})
	if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	if !errors.Is(err, ErrCorruptData) {
		t.Fatalf("任一调整请求损坏都应返回 ErrCorruptData，得到 %v", err)
	}
	if s != nil {
		s.Close()
		t.Fatalf("损坏台账不应返回可用的 Store 对象")
	}
	if !strings.Contains(err.Error(), "u-second") {
		t.Fatalf("错误信息应指出问题请求编号 u-second，得到 %v", err)
	}
}

// 白盒核对：正常台账中 u-first 的保存结果确实是第一次调整时的内容，且本
// 文件的改坏辅助函数确实改动了文件内容（防止测试因辅助函数失效而假性通过）。
func TestUpdateDraftCorruptionHelpersWork(t *testing.T) {
	dir := t.TempDir()
	good := setupUpdateDraftRequestLedger(t, dir)

	var saved BatchView
	if err := json.Unmarshal(updateResultRaw(t, good, "u-first"), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.BatchNo != "B1" || saved.RecipeNo != "R1" || saved.RecipeVersion != "v1" ||
		saved.RecipeName != "配方初版" || saved.PlannedPortions != 8 ||
		saved.Status != StatusDraft || len(saved.Feedings) != 0 || len(saved.Materials) != 2 {
		t.Fatalf("正常台账中保存的调整结果应为第一次调整内容，得到 %+v", saved)
	}
	bad := corruptUpdateRequestResult(t, good, "u-first", func() json.RawMessage {
		return json.RawMessage(`{}`)
	})
	if bytes.Equal(good, bad) {
		t.Fatal("改坏辅助函数未改动台账内容")
	}
	// 正常台账本身必须能打开：所有“正常后续操作不误判”的前提。
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("正常台账应能打开: %v", err)
	}
	s.Close()
}
