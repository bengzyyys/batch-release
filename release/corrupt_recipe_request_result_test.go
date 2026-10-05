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

// 本文件是“已保存的成功配方登记请求的返回结果必须对应原请求确实登记的
// 配方版本”的回归保障：配方登记成功后，用原请求编号、原内容再次提交，
// 只能取回第一次成功登记的那个版本。请求记录里保存的结果即使变成 null、
// 空对象，或被替换成另一版本（哪怕同编号、同名称、同用量）的完整结果，
// 也不能被当作成功结果返回——打开台账及之后的每次查询/写入重载，都必须
// 核对原提交内容、保存结果与实际登记版本三者一致（配方编号、版本号、名称、
// 物料项数、排列顺序、各项编号与每份克数）；不一致按 ErrCorruptData 拒绝
// 整份台账，不删除请求、不补造版本，也不重新登记。

// setupRecipeRequestLedger 在 dir 建立一份只有配方、没有批次的正常台账并
// 关闭（因此所有登记版本都尚未被批次采用）：
//   - recipe-1 登记 R1/v1「标准配方」：M1=100、M2=0.5、M3=0.010；
//   - recipe-2 登记 R1/v2「改版配方」：M1=250、M4=2；
//   - recipe-3 登记 R3/v1「标准配方」：物料与用量与 R1/v1 完全相同。
//
// 返回落盘后的台账文件内容，供各用例改坏后重写。
func setupRecipeRequestLedger(t *testing.T, dir string) []byte {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	registerStandardRecipe(t, s, "recipe-1")
	if _, err := s.RegisterRecipe("recipe-2", "R1", "v2", "改版配方", []MaterialInput{
		{MaterialNo: "M1", Grams: "250"},
		{MaterialNo: "M4", Grams: "2"},
	}); err != nil {
		t.Fatal(err)
	}
	// 与 R1/v1 名称、物料、用量完全相同的另一版本：名称或用量相同也不能
	// 让两个版本互为顶替。
	if _, err := s.RegisterRecipe("recipe-3", "R3", "v1", "标准配方", []MaterialInput{
		{MaterialNo: "M1", Grams: "100"},
		{MaterialNo: "M2", Grams: "0.5"},
		{MaterialNo: "M3", Grams: "0.010"},
	}); err != nil {
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

// corruptRecipeRequestResult 把正常台账内容中 recipe-1 请求的保存结果替换为
// mutate 给出的值（mutate 返回 nil 表示删除该字段），返回改写后的文件内容。
func corruptRecipeRequestResult(t *testing.T, good []byte, mutate func() json.RawMessage) []byte {
	t.Helper()
	var st persistedState
	if err := json.Unmarshal(good, &st); err != nil {
		t.Fatal(err)
	}
	req := st.Requests["recipe-1"]
	if req == nil {
		t.Fatalf("正常台账中应存在 recipe-1 请求记录")
	}
	req.Result = mutate()
	data, err := json.MarshalIndent(&st, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// marshalRecipeView 把一份配方视图序列化为保存结果使用的 JSON。
func marshalRecipeView(t *testing.T, v RecipeView) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func r1v1ResultView() RecipeView {
	return RecipeView{
		RecipeNo: "R1", Version: "v1", Name: "标准配方",
		Materials: []MaterialView{
			{MaterialNo: "M1", Grams: "100"},
			{MaterialNo: "M2", Grams: "0.5"},
			{MaterialNo: "M3", Grams: "0.01"},
		},
	}
}

// 已保存的成功配方登记请求的结果缺失、为 null、为空对象、被替换成另一版本、
// 关键字段缺失，或与实际登记版本/原提交内容在名称、物料编号、克数、顺序、
// 项数上不符时，Open 必须返回 ErrCorruptData：错误信息指出问题请求编号及
// 关联的配方编号、版本号，不返回可用的台账对象，也不改写原文件。台账中
// 没有任何批次（该版本尚未被采用）也不能绕过。
func TestOpenRejectsCorruptRecipeRequestResult(t *testing.T) {
	cases := []struct {
		name   string
		mutate func() json.RawMessage
	}{
		{"结果缺失", func() json.RawMessage { return nil }},
		{"结果为 null", func() json.RawMessage { return json.RawMessage("null") }},
		{"结果为空对象", func() json.RawMessage { return json.RawMessage(`{}`) }},
		{"结果不是配方对象", func() json.RawMessage { return json.RawMessage(`"not-an-object"`) }},
		{"结果缺配方编号", func() json.RawMessage {
			v := r1v1ResultView()
			v.RecipeNo = ""
			return marshalRecipeView(t, v)
		}},
		{"结果缺版本号", func() json.RawMessage {
			v := r1v1ResultView()
			v.Version = ""
			return marshalRecipeView(t, v)
		}},
		{"结果缺名称", func() json.RawMessage {
			v := r1v1ResultView()
			v.Name = ""
			return marshalRecipeView(t, v)
		}},
		{"结果物料列表为空", func() json.RawMessage {
			v := r1v1ResultView()
			v.Materials = nil
			return marshalRecipeView(t, v)
		}},
		{"结果被替换成同编号另一版本", func() json.RawMessage {
			// R1/v2 真实存在且内容完整，但它属于 recipe-2，不能顶替 recipe-1 的登记。
			return marshalRecipeView(t, RecipeView{
				RecipeNo: "R1", Version: "v2", Name: "改版配方",
				Materials: []MaterialView{
					{MaterialNo: "M1", Grams: "250"},
					{MaterialNo: "M4", Grams: "2"},
				},
			})
		}},
		{"结果被替换成名称用量都相同的另一编号版本", func() json.RawMessage {
			// R3/v1 与 R1/v1 名称、物料、用量完全相同，仍不是 recipe-1 登记的版本。
			return marshalRecipeView(t, RecipeView{
				RecipeNo: "R3", Version: "v1", Name: "标准配方",
				Materials: []MaterialView{
					{MaterialNo: "M1", Grams: "100"},
					{MaterialNo: "M2", Grams: "0.5"},
					{MaterialNo: "M3", Grams: "0.01"},
				},
			})
		}},
		{"结果编号版本被改成另一版本但物料照抄v1", func() json.RawMessage {
			v := r1v1ResultView()
			v.Version = "v2" // 编号仍为 R1、物料仍是 v1 的内容，版本字段对不上即损坏
			return marshalRecipeView(t, v)
		}},
		{"结果名称与实际登记不符", func() json.RawMessage {
			v := r1v1ResultView()
			v.Name = "本地改名"
			return marshalRecipeView(t, v)
		}},
		{"结果物料编号与实际登记不符", func() json.RawMessage {
			v := r1v1ResultView()
			v.Materials[0].MaterialNo = "M9"
			return marshalRecipeView(t, v)
		}},
		{"结果每份克数与实际登记不符", func() json.RawMessage {
			v := r1v1ResultView()
			v.Materials[0].Grams = "101"
			return marshalRecipeView(t, v)
		}},
		{"结果调换物料顺序", func() json.RawMessage {
			v := r1v1ResultView()
			v.Materials[0], v.Materials[1] = v.Materials[1], v.Materials[0]
			return marshalRecipeView(t, v)
		}},
		{"结果删减一项物料", func() json.RawMessage {
			v := r1v1ResultView()
			v.Materials = v.Materials[:2]
			return marshalRecipeView(t, v)
		}},
		{"结果追加一项物料", func() json.RawMessage {
			v := r1v1ResultView()
			v.Materials = append(v.Materials, MaterialView{MaterialNo: "M9", Grams: "9"})
			return marshalRecipeView(t, v)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			good := setupRecipeRequestLedger(t, dir)
			bad := corruptRecipeRequestResult(t, good, tc.mutate)
			if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
				t.Fatal(err)
			}

			s, err := Open(dir)
			if !errors.Is(err, ErrCorruptData) {
				t.Fatalf("保存结果损坏应返回 ErrCorruptData，得到 %v", err)
			}
			if s != nil {
				s.Close()
				t.Fatalf("损坏台账不应返回可用的 Store 对象")
			}
			msg := err.Error()
			for _, want := range []string{"recipe-1", "R1", "v1"} {
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

// 原请求登记的版本在台账中不存在时，保存的结果不能单独作为登记成功的
// 依据：即使结果本身完整、台账里另有完整的同编号其他版本（R1/v2），也按
// 损坏处理，不能用 v2 顶替，也不补造 v1。
func TestOpenRejectsRecipeRequestWhenVersionMissing(t *testing.T) {
	dir := t.TempDir()
	good := setupRecipeRequestLedger(t, dir)
	var st persistedState
	if err := json.Unmarshal(good, &st); err != nil {
		t.Fatal(err)
	}
	// 删除 R1/v1（recipe-1 登记的版本），保留完整的 R1/v2、R3/v1 及其请求。
	kept := st.Recipes[:0]
	for _, r := range st.Recipes {
		if !(r.RecipeNo == "R1" && r.Version == "v1") {
			kept = append(kept, r)
		}
	}
	st.Recipes = kept
	bad, err := json.MarshalIndent(&st, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	if !errors.Is(err, ErrCorruptData) {
		t.Fatalf("原版本不存在但保留请求结果应返回 ErrCorruptData，得到 %v", err)
	}
	if s != nil {
		s.Close()
		t.Fatalf("损坏台账不应返回可用的 Store 对象")
	}
	msg := err.Error()
	for _, want := range []string{"recipe-1", "R1", "v1"} {
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

// 实际登记版本被改坏（名称、物料编号、每份克数、顺序、项数）而保存结果仍
// 完整时，同样按损坏处理：保存结果必须对应实际登记版本，两者不能只居其一。
func TestOpenRejectsRecipeRequestWhenActualVersionTampered(t *testing.T) {
	cases := []struct {
		name   string
		tamper func(*recipeRecord)
	}{
		{"实际版本名称被改", func(r *recipeRecord) { r.Name = "被改名" }},
		{"实际版本物料编号被改", func(r *recipeRecord) { r.Materials[0].MaterialNo = "M9" }},
		{"实际版本每份克数被改", func(r *recipeRecord) { r.Materials[0].GramsMilli = 101000 }},
		{"实际版本物料顺序被调换", func(r *recipeRecord) {
			r.Materials[0], r.Materials[1] = r.Materials[1], r.Materials[0]
		}},
		{"实际版本物料被删减", func(r *recipeRecord) { r.Materials = r.Materials[:2] }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			good := setupRecipeRequestLedger(t, dir)
			var st persistedState
			if err := json.Unmarshal(good, &st); err != nil {
				t.Fatal(err)
			}
			rec := findRecipe(&st, "R1", "v1")
			if rec == nil {
				t.Fatal("正常台账中应存在 R1/v1")
			}
			tc.tamper(rec)
			bad, err := json.MarshalIndent(&st, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
				t.Fatal(err)
			}

			s, err := Open(dir)
			if !errors.Is(err, ErrCorruptData) {
				t.Fatalf("实际版本与保存结果不符应返回 ErrCorruptData，得到 %v", err)
			}
			if s != nil {
				s.Close()
				t.Fatalf("损坏台账不应返回可用的 Store 对象")
			}
			msg := err.Error()
			for _, want := range []string{"recipe-1", "R1", "v1"} {
				if !strings.Contains(msg, want) {
					t.Fatalf("错误信息应包含 %q，得到 %v", want, err)
				}
			}
		})
	}
}

// 原提交内容无法解析时也按损坏处理，而不是当成新请求重新登记。
func TestOpenRejectsRecipeRequestWithUnparsablePayload(t *testing.T) {
	dir := t.TempDir()
	good := setupRecipeRequestLedger(t, dir)
	var st persistedState
	if err := json.Unmarshal(good, &st); err != nil {
		t.Fatal(err)
	}
	st.Requests["recipe-1"].Payload = "{不是合法JSON"
	bad, err := json.MarshalIndent(&st, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if !errors.Is(err, ErrCorruptData) {
		t.Fatalf("提交内容无法解析应返回 ErrCorruptData，得到 %v", err)
	}
	if s != nil {
		s.Close()
		t.Fatalf("损坏台账不应返回可用的 Store 对象")
	}
}

// 正常写法差异不能误判为损坏：保存结果的每份克数写成 100.000、0.500、
// 0.010，与原提交及实际版本的 100、0.5、0.01 是相同用量。台账应正常打开，
// 重放仍返回第一次成功登记的结果（按系统规范显示为去末尾零的写法）。
func TestOpenAcceptsRecipeRequestResultGramsFormatVariants(t *testing.T) {
	dir := t.TempDir()
	good := setupRecipeRequestLedger(t, dir)

	var st persistedState
	if err := json.Unmarshal(good, &st); err != nil {
		t.Fatal(err)
	}
	var result RecipeView
	if err := json.Unmarshal(st.Requests["recipe-1"].Result, &result); err != nil {
		t.Fatal(err)
	}
	// 系统保存的结果已去掉末尾零。
	wantGrams := []string{"100", "0.5", "0.01"}
	for i, m := range result.Materials {
		if m.Grams != wantGrams[i] {
			t.Fatalf("系统保存的第 %d 项克数应为 %q，得到 %q", i+1, wantGrams[i], m.Grams)
		}
		m.Grams = map[string]string{"100": "100.000", "0.5": "0.500", "0.01": "0.010"}[m.Grams]
	}
	st.Requests["recipe-1"].Result = marshalRecipeView(t, result)
	data, err := json.MarshalIndent(&st, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stateFileName), data, 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("克数写法差异不应判为损坏: %v", err)
	}
	defer s.Close()

	// 重放取回第一次成功登记的版本：编号、版本、名称、物料顺序与用量不变。
	replay, err := s.RegisterRecipe("recipe-1", "R1", "v1", "标准配方", []MaterialInput{
		{MaterialNo: "M1", Grams: "100"},
		{MaterialNo: "M2", Grams: "0.5"},
		{MaterialNo: "M3", Grams: "0.010"},
	})
	if err != nil {
		t.Fatalf("重放应成功: %v", err)
	}
	assertRegisteredStandardR1V1(t, replay)

	got, err := s.GetRecipe("R1", "v1")
	if err != nil {
		t.Fatal(err)
	}
	assertRegisteredStandardR1V1(t, got)
}

// assertRegisteredStandardR1V1 断言视图是 recipe-1 登记的 R1/v1「标准配方」。
func assertRegisteredStandardR1V1(t *testing.T, r *RecipeView) {
	t.Helper()
	assertRecipeViewExactly(t, r, "R1", "v1", "标准配方", []MaterialView{
		{MaterialNo: "M1", Grams: "100"},
		{MaterialNo: "M2", Grams: "0.5"},
		{MaterialNo: "M3", Grams: "0.01"},
	})
}

// 台账正常打开后，保存内容中的配方登记请求结果被改坏：下一次查询或写入
// 必须返回 ErrCorruptData——即使本次查询的是另一条正常版本，或写入另一个
// 新配方，也不能忽略损坏的请求；被拒绝的重放不得返回损坏的保存结果，被
// 拒绝的写入不留业务变化、不占用请求编号，原台账内容不变。恢复后原成功
// 请求仍可幂等重放，被拒绝过的请求编号可以正常使用。
func TestRecipeRequestResultCorruptionAfterOpenDetectedOnNextAccess(t *testing.T) {
	dir := t.TempDir()
	good := setupRecipeRequestLedger(t, dir)

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// 打开后把 recipe-1 的保存结果改坏为空对象（其余记录保持完整）。
	bad := corruptRecipeRequestResult(t, good, func() json.RawMessage {
		return json.RawMessage(`{}`)
	})
	if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
		t.Fatal(err)
	}

	// 查询损坏版本与正常版本都必须失败，不能沿用此前读到的内容。
	for _, key := range []struct {
		no, ver string
	}{{"R1", "v1"}, {"R1", "v2"}, {"R3", "v1"}} {
		if v, err := s.GetRecipe(key.no, key.ver); !errors.Is(err, ErrCorruptData) {
			t.Fatalf("查询配方 %q/%q 应返回 ErrCorruptData，得到 %v", key.no, key.ver, err)
		} else if v != nil {
			t.Fatalf("被拒绝的查询不应返回部分台账视图: %+v", v)
		}
	}
	// 用原编号、原内容重放损坏的请求：不得把损坏的保存结果当成成功结果
	// 返回，必须报损坏。
	if _, err := s.RegisterRecipe("recipe-1", "R1", "v1", "标准配方", []MaterialInput{
		{MaterialNo: "M1", Grams: "100"},
		{MaterialNo: "M2", Grams: "0.5"},
		{MaterialNo: "M3", Grams: "0.010"},
	}); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("重放损坏的配方登记请求应返回 ErrCorruptData，得到 %v", err)
	}
	// 与损坏请求无关的新写入同样不能绕过。
	if _, err := s.RegisterRecipe("recipe-new", "R9", "v1", "新配方", []MaterialInput{
		{MaterialNo: "M1", Grams: "1"},
	}); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上登记新配方应返回 ErrCorruptData，得到 %v", err)
	}

	// 被拒绝的访问不改动文件、不占用请求编号。
	after, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, bad) {
		t.Fatalf("被拒绝的访问不应改动台账文件")
	}
	if bytes.Contains(after, []byte("recipe-new")) || bytes.Contains(after, []byte(`"R9"`)) {
		t.Fatalf("被拒绝的写入不应留下请求记录或业务记录")
	}

	// 恢复后：原成功请求仍取回第一次登记的结果，版本保持原样；被拒绝过
	// 的请求编号可以正常用于一次新的合法登记。
	if err := os.WriteFile(filepath.Join(dir, stateFileName), good, 0o644); err != nil {
		t.Fatal(err)
	}
	replay, err := s.RegisterRecipe("recipe-1", "R1", "v1", "标准配方", []MaterialInput{
		{MaterialNo: "M1", Grams: "100"},
		{MaterialNo: "M2", Grams: "0.5"},
		{MaterialNo: "M3", Grams: "0.010"},
	})
	if err != nil {
		t.Fatalf("恢复后重放原成功请求失败: %v", err)
	}
	assertRegisteredStandardR1V1(t, replay)
	newRecipe, err := s.RegisterRecipe("recipe-new", "R9", "v1", "新配方", []MaterialInput{
		{MaterialNo: "M1", Grams: "1"},
	})
	if err != nil {
		t.Fatalf("被拒绝过的请求编号应仍可合法提交: %v", err)
	}
	if newRecipe.RecipeNo != "R9" || len(newRecipe.Materials) != 1 || newRecipe.Materials[0].Grams != "1" {
		t.Fatalf("新配方登记结果不正确: %+v", newRecipe)
	}
}

// 读取核对规则不改变请求内容匹配：正常台账中，用同一编号把原来的 1.000
// 改成数值相等的 1 再提交，仍返回 ErrRequestConflict；原内容重放仍取回
// 第一次的结果（克数显示为 1），台账中始终只有第一次登记的版本。
func TestRecipeRequestResultCheckKeepsPayloadMatching(t *testing.T) {
	dir := t.TempDir()
	s1, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s1.RegisterRecipe("r-1", "R1", "v1", "配方一", []MaterialInput{
		{MaterialNo: "M1", Grams: "1.000"},
		{MaterialNo: "M2", Grams: "0.500"},
	}); err != nil {
		t.Fatalf("登记配方失败: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// 只改克数写法（数值相等）也算不同内容：不能借核对数值相等视为原请求。
	if _, err := s.RegisterRecipe("r-1", "R1", "v1", "配方一", []MaterialInput{
		{MaterialNo: "M1", Grams: "1"},
		{MaterialNo: "M2", Grams: "0.5"},
	}); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("1.000 改成数值相等的 1 应返回 ErrRequestConflict，得到 %v", err)
	}
	// 名称或物料的其他数值相等改写同样是冲突，而不是重放。
	if _, err := s.RegisterRecipe("r-1", "R1", "v1", "配方一", []MaterialInput{
		{MaterialNo: "M1", Grams: "1.0"},
		{MaterialNo: "M2", Grams: "0.500"},
	}); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("1.000 改成 1.0 应返回 ErrRequestConflict，得到 %v", err)
	}

	// 原内容重放成功，返回第一次登记的结果；版本没有被新增或覆盖。
	replay, err := s.RegisterRecipe("r-1", "R1", "v1", "配方一", []MaterialInput{
		{MaterialNo: "M1", Grams: "1.000"},
		{MaterialNo: "M2", Grams: "0.500"},
	})
	if err != nil {
		t.Fatalf("原内容重放应成功: %v", err)
	}
	assertRecipeViewExactly(t, replay, "R1", "v1", "配方一", []MaterialView{
		{MaterialNo: "M1", Grams: "1"},
		{MaterialNo: "M2", Grams: "0.5"},
	})
	got, err := s.GetRecipe("R1", "v1")
	if err != nil {
		t.Fatal(err)
	}
	assertRecipeViewExactly(t, got, "R1", "v1", "配方一", []MaterialView{
		{MaterialNo: "M1", Grams: "1"},
		{MaterialNo: "M2", Grams: "0.5"},
	})
}

// 正常数据仍按既有幂等功能使用：原编号与原内容重复提交返回首次结果；
// 即使之后又登记了同编号的其他版本，重放也不会返回新版本的内容。
func TestRecipeRequestReplayStillReturnsFirstResult(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	first, err := s.RegisterRecipe("recipe-1", "R1", "v1", "配方一", []MaterialInput{
		{MaterialNo: "M1", Grams: "100"},
		{MaterialNo: "M2", Grams: "0.5"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterRecipe("recipe-2", "R1", "v2", "配方改版", []MaterialInput{
		{MaterialNo: "M1", Grams: "250"},
	}); err != nil {
		t.Fatal(err)
	}

	replay, err := s.RegisterRecipe("recipe-1", "R1", "v1", "配方一", []MaterialInput{
		{MaterialNo: "M1", Grams: "100"},
		{MaterialNo: "M2", Grams: "0.5"},
	})
	if err != nil {
		t.Fatalf("原内容重放应成功: %v", err)
	}
	assertRecipeViewExactly(t, replay, first.RecipeNo, first.Version, first.Name, first.Materials)
	v2, err := s.GetRecipe("R1", "v2")
	if err != nil {
		t.Fatal(err)
	}
	if v2.Name != "配方改版" || len(v2.Materials) != 1 || v2.Materials[0].Grams != "250" {
		t.Fatalf("v2 登记不应受重放影响: %+v", v2)
	}

	// 关闭重开后重放结果不变。
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	replay2, err := s2.RegisterRecipe("recipe-1", "R1", "v1", "配方一", []MaterialInput{
		{MaterialNo: "M1", Grams: "100"},
		{MaterialNo: "M2", Grams: "0.5"},
	})
	if err != nil {
		t.Fatalf("重开后重放应成功: %v", err)
	}
	assertRecipeViewExactly(t, replay2, "R1", "v1", "配方一", []MaterialView{
		{MaterialNo: "M1", Grams: "100"},
		{MaterialNo: "M2", Grams: "0.5"},
	})
}
