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
// 版本”的回归保障：登记配方版本成功后，按原请求编号、原内容再次提交，
// 只能取回第一次成功登记的那个版本。台账里保存的请求结果即使变成 null、
// 空对象，或被替换成另一版本的完整结果，也不能被当作成功结果返回——打开
// 台账及之后每次查询/写入重载，都必须核对原提交内容、保存结果与实际登记
// 版本三者一致（配方编号、版本号、名称、物料项数、排列顺序、各项物料编号
// 与每份克数）；不一致按 ErrCorruptData 拒绝整份台账，不删除请求、不补造
// 版本，也不重新登记。

// setupRecipeRequestLedger 在 dir 建立一份正常台账并关闭：
//   - r1 登记 R1/v1「配方一」：M1=1.000 克、M2=0.500 克（结果分别显示为 1、0.5）；
//   - r2 登记 R1/v2「配方二」：M1=2 克（不同版本）；
//   - r3 登记 R1/v3「配方一」：与 R1/v1 名称、用量完全相同，仅版本号不同。
//
// 台账中没有任何批次：R1/v1 尚未被批次采用，用于验证“版本未被采用也不能
// 跳过核对”。返回落盘后的台账文件内容，供各用例改坏后重写。
func setupRecipeRequestLedger(t *testing.T, dir string) []byte {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	if _, err := s.RegisterRecipe("r1", "R1", "v1", "配方一", []MaterialInput{
		{MaterialNo: "M1", Grams: "1.000"},
		{MaterialNo: "M2", Grams: "0.500"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterRecipe("r2", "R1", "v2", "配方二", []MaterialInput{
		{MaterialNo: "M1", Grams: "2"},
	}); err != nil {
		t.Fatal(err)
	}
	// 与 v1 名称、用量完全相同、仅版本号不同的另一版本：用于证明“名称或
	// 用量相同也不算同一版本”。
	if _, err := s.RegisterRecipe("r3", "R1", "v3", "配方一", []MaterialInput{
		{MaterialNo: "M1", Grams: "1.000"},
		{MaterialNo: "M2", Grams: "0.500"},
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

// corruptRecipeRequestResult 把正常台账内容中指定配方登记请求的保存结果替换为
// mutate 给出的值（mutate 返回 nil 表示删除该字段），返回改写后的文件内容。
func corruptRecipeRequestResult(t *testing.T, good []byte, reqNo string, mutate func() json.RawMessage) []byte {
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

// marshalRecipeView 把一份配方视图序列化为保存结果使用的 JSON。
func marshalRecipeView(t *testing.T, v RecipeView) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// v1ResultView 是 r1 第一次成功登记 R1/v1 时应有的结果视图（克数按规范化写法）。
func v1ResultView() RecipeView {
	return RecipeView{
		RecipeNo: "R1",
		Version:  "v1",
		Name:     "配方一",
		Materials: []MaterialView{
			{MaterialNo: "M1", Grams: "1"},
			{MaterialNo: "M2", Grams: "0.5"},
		},
	}
}

// 已保存的成功配方登记请求结果缺失、为 null、为空对象、被替换成另一版本
// （即使名称与用量完全相同），或编号、版本、名称、物料项数、顺序、物料编号、
// 每份克数与实际版本或原提交内容不符时，Open 必须返回 ErrCorruptData：
// 错误信息指出问题请求编号及关联的配方编号、版本号，不返回可用台账，也不
// 改写原文件。该版本尚未被任何批次采用也不能绕过。
func TestOpenRejectsCorruptRecipeRequestResult(t *testing.T) {
	cases := []struct {
		name   string
		mutate func() json.RawMessage
	}{
		{"结果缺失", func() json.RawMessage { return nil }},
		{"结果为 null", func() json.RawMessage { return json.RawMessage("null") }},
		{"结果为空对象", func() json.RawMessage { return json.RawMessage(`{}`) }},
		{"结果不是配方对象", func() json.RawMessage { return json.RawMessage(`"not-an-object"`) }},
		{"结果物料列表为 null", func() json.RawMessage {
			v := v1ResultView()
			v.Materials = nil
			return marshalRecipeView(t, v)
		}},
		{"结果物料列表为空数组", func() json.RawMessage {
			v := v1ResultView()
			v.Materials = []MaterialView{}
			return marshalRecipeView(t, v)
		}},
		{"结果缺配方名称", func() json.RawMessage {
			v := v1ResultView()
			v.Name = ""
			return marshalRecipeView(t, v)
		}},
		{"结果被替换成同编号另一版本v2", func() json.RawMessage {
			// R1/v2 完整存在，但它属于 r2，不是 r1 登记的版本。
			return marshalRecipeView(t, RecipeView{
				RecipeNo:  "R1",
				Version:   "v2",
				Name:      "配方二",
				Materials: []MaterialView{{MaterialNo: "M1", Grams: "2"}},
			})
		}},
		{"结果被替换成名称用量完全相同的另一版本", func() json.RawMessage {
			// R1/v3 与 R1/v1 名称、物料、用量完全一致，仅版本号不同：
			// 仍不能用 v3 的结果顶替 v1。
			return marshalRecipeView(t, RecipeView{
				RecipeNo: "R1",
				Version:  "v3",
				Name:     "配方一",
				Materials: []MaterialView{
					{MaterialNo: "M1", Grams: "1"},
					{MaterialNo: "M2", Grams: "0.5"},
				},
			})
		}},
		{"结果配方编号与原提交不符", func() json.RawMessage {
			v := v1ResultView()
			v.RecipeNo = "R9"
			return marshalRecipeView(t, v)
		}},
		{"结果版本号与原提交不符", func() json.RawMessage {
			v := v1ResultView()
			v.Version = "v9"
			return marshalRecipeView(t, v)
		}},
		{"结果名称与实际版本不符", func() json.RawMessage {
			v := v1ResultView()
			v.Name = "配方被改名"
			return marshalRecipeView(t, v)
		}},
		{"结果多出一项物料", func() json.RawMessage {
			v := v1ResultView()
			v.Materials = append(v.Materials, MaterialView{MaterialNo: "M3", Grams: "3"})
			return marshalRecipeView(t, v)
		}},
		{"结果少了一项物料", func() json.RawMessage {
			v := v1ResultView()
			v.Materials = v.Materials[:1]
			return marshalRecipeView(t, v)
		}},
		{"结果调换了物料顺序", func() json.RawMessage {
			return marshalRecipeView(t, RecipeView{
				RecipeNo: "R1",
				Version:  "v1",
				Name:     "配方一",
				Materials: []MaterialView{
					{MaterialNo: "M2", Grams: "0.5"},
					{MaterialNo: "M1", Grams: "1"},
				},
			})
		}},
		{"结果物料编号与实际版本不符", func() json.RawMessage {
			v := v1ResultView()
			v.Materials[0].MaterialNo = "M9"
			return marshalRecipeView(t, v)
		}},
		{"结果每份克数与实际版本不符", func() json.RawMessage {
			v := v1ResultView()
			v.Materials[0].Grams = "2"
			return marshalRecipeView(t, v)
		}},
		{"结果克数写法不合法", func() json.RawMessage {
			v := v1ResultView()
			v.Materials[0].Grams = "abc"
			return marshalRecipeView(t, v)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			good := setupRecipeRequestLedger(t, dir)
			bad := corruptRecipeRequestResult(t, good, "r1", tc.mutate)
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
			for _, want := range []string{"r1", "R1", "v1"} {
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

// 原请求对应的版本不存在时，保存的结果不能单独作为登记成功的依据：
// 即使结果本身完整、台账里另有同编号的完整版本 R1/v2、R1/v3，也按损坏处理。
func TestOpenRejectsRecipeRequestWhenVersionMissing(t *testing.T) {
	dir := t.TempDir()
	good := setupRecipeRequestLedger(t, dir)
	var st persistedState
	if err := json.Unmarshal(good, &st); err != nil {
		t.Fatal(err)
	}
	// 删除 r1 登记的 R1/v1，保留 r1 请求及其完整结果，另保留 R1/v2、R1/v3。
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
		t.Fatalf("原登记版本不存在但保留请求结果应返回 ErrCorruptData，得到 %v", err)
	}
	if s != nil {
		s.Close()
		t.Fatalf("损坏台账不应返回可用的 Store 对象")
	}
	msg := err.Error()
	for _, want := range []string{"r1", "R1", "v1"} {
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

// 保存结果与原提交内容都完好，但实际登记版本被改坏（名称、物料编号、每份
// 克数、顺序发生仍能通过配方结构校验的漂移）：查询与重放都可能据此返回与
// 第一次登记不同的配方，Open 必须按损坏拒绝。
func TestOpenRejectsRecipeRequestWhenActualVersionDrifted(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*recipeRecord)
	}{
		{"实际版本名称被改", func(r *recipeRecord) { r.Name = "配方被改名" }},
		{"实际版本物料编号被改", func(r *recipeRecord) { r.Materials[0].MaterialNo = "M9" }},
		{"实际版本每份克数被改", func(r *recipeRecord) { r.Materials[0].GramsMilli = 2000 }},
		{"实际版本物料顺序被调换", func(r *recipeRecord) {
			r.Materials[0], r.Materials[1] = r.Materials[1], r.Materials[0]
		}},
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
			tc.mutate(rec)
			bad, err := json.MarshalIndent(&st, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
				t.Fatal(err)
			}

			s, err := Open(dir)
			if !errors.Is(err, ErrCorruptData) {
				t.Fatalf("实际版本与保存结果/原提交不一致应返回 ErrCorruptData，得到 %v", err)
			}
			if s != nil {
				s.Close()
				t.Fatalf("损坏台账不应返回可用的 Store 对象")
			}
			msg := err.Error()
			for _, want := range []string{"r1", "R1", "v1"} {
				if !strings.Contains(msg, want) {
					t.Fatalf("错误信息应包含 %q，得到 %v", want, err)
				}
			}
		})
	}
}

// 原提交内容无法解析时无法确定关联版本：仍按损坏拒绝，错误信息至少指出
// 问题请求编号。
func TestOpenRejectsRecipeRequestWithUnparsablePayload(t *testing.T) {
	dir := t.TempDir()
	good := setupRecipeRequestLedger(t, dir)
	var st persistedState
	if err := json.Unmarshal(good, &st); err != nil {
		t.Fatal(err)
	}
	st.Requests["r1"].Payload = "{不是合法JSON"
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
	if !strings.Contains(err.Error(), "r1") {
		t.Fatalf("错误信息应指出问题请求编号 r1，得到 %v", err)
	}
}

// 正常写法差异不能误判为损坏：原请求填写 1.000、0.500，系统保存的结果显示
// 1、0.5；即使保存结果改写成 1.000、0.500，换算成相同每份用量也合法。
// 台账应正常打开，重放仍返回第一次成功登记的结果，不新增或覆盖版本。
func TestOpenAcceptsRecipeRequestResultFormatVariants(t *testing.T) {
	dir := t.TempDir()
	good := setupRecipeRequestLedger(t, dir)

	// 系统保存的结果克数应已规范化为 1、0.5（原提交为 1.000、0.500）。
	var st persistedState
	if err := json.Unmarshal(good, &st); err != nil {
		t.Fatal(err)
	}
	var saved RecipeView
	if err := json.Unmarshal(st.Requests["r1"].Result, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Materials[0].Grams != "1" || saved.Materials[1].Grams != "0.5" {
		t.Fatalf("系统保存的结果克数应为 1、0.5，得到 %q、%q",
			saved.Materials[0].Grams, saved.Materials[1].Grams)
	}
	// 把保存结果的克数改写为带末尾零的等值写法，应仍被接受。
	st.Requests["r1"].Result = marshalRecipeView(t, RecipeView{
		RecipeNo: "R1",
		Version:  "v1",
		Name:     "配方一",
		Materials: []MaterialView{
			{MaterialNo: "M1", Grams: "1.000"},
			{MaterialNo: "M2", Grams: "0.500"},
		},
	})
	data, err := json.MarshalIndent(&st, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stateFileName), data, 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("等值克数写法不应判为损坏: %v", err)
	}
	defer s.Close()

	// 原编号、原内容重放：取回第一次成功登记的 R1/v1。保存结果的克数字面
	// 写法虽被改成 1.000、0.500，但表示的每份用量与实际版本一致，重放成功；
	// 按实际克数核对，不因末尾零不同而把同一用量当成不同配方。
	replay, err := s.RegisterRecipe("r1", "R1", "v1", "配方一", []MaterialInput{
		{MaterialNo: "M1", Grams: "1.000"},
		{MaterialNo: "M2", Grams: "0.500"},
	})
	if err != nil {
		t.Fatalf("重放应成功: %v", err)
	}
	if replay.RecipeNo != "R1" || replay.Version != "v1" || replay.Name != "配方一" {
		t.Fatalf("重放结果编号/版本/名称不正确: %+v", replay)
	}
	if len(replay.Materials) != 2 {
		t.Fatalf("重放结果应含 2 项物料，得到 %d 项", len(replay.Materials))
	}
	for i, wantMilli := range []gramsMilli{1000, 500} {
		got, err := parseGrams(replay.Materials[i].Grams)
		if err != nil {
			t.Fatalf("重放结果第 %d 项克数 %q 不合法", i+1, replay.Materials[i].Grams)
		}
		if got != wantMilli {
			t.Fatalf("重放结果第 %d 项每份用量应等于 %s 克，得到 %q",
				i+1, wantMilli, replay.Materials[i].Grams)
		}
	}

	// 台账中实际登记的版本仍按规范化写法显示，且查询与重放指向同一版本、
	// 同一用量；重放不新增或覆盖版本，三个版本都在。
	got, err := s.GetRecipe("R1", "v1")
	if err != nil {
		t.Fatalf("R1/v1 查询失败: %v", err)
	}
	assertRecipeViewExactly(t, got, "R1", "v1", "配方一", v1ResultView().Materials)
	for _, key := range []struct{ no, ver string }{{"R1", "v2"}, {"R1", "v3"}} {
		if _, err := s.GetRecipe(key.no, key.ver); err != nil {
			t.Fatalf("%s/%s 应仍存在: %v", key.no, key.ver, err)
		}
	}
}

// 台账正常打开后，保存内容中的配方登记请求结果被改坏：下一次查询或写入必须
// 返回 ErrCorruptData——即使本次访问的是另一条正常版本，也不能忽略损坏的
// 请求；被拒绝的重放不得返回损坏结果，被拒绝的写入不留业务变化、不占用请求
// 编号，原台账内容不变。恢复后原成功请求仍可幂等重放，被拒绝过的编号可用。
func TestRecipeRequestResultCorruptionAfterOpenDetectedOnNextAccess(t *testing.T) {
	dir := t.TempDir()
	good := setupRecipeRequestLedger(t, dir)

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// 打开后把 r1 的保存结果改坏为空对象（其余版本与请求保持完整）。
	bad := corruptRecipeRequestResult(t, good, "r1", func() json.RawMessage {
		return json.RawMessage(`{}`)
	})
	if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
		t.Fatal(err)
	}

	// 查询损坏版本与正常版本都必须失败，不能沿用此前读到的内容。
	for _, key := range []struct{ no, ver string }{
		{"R1", "v1"}, {"R1", "v2"}, {"R1", "v3"},
	} {
		if v, err := s.GetRecipe(key.no, key.ver); !errors.Is(err, ErrCorruptData) {
			t.Fatalf("查询配方 %q/%q 应返回 ErrCorruptData，得到 %v", key.no, key.ver, err)
		} else if v != nil {
			t.Fatalf("被拒绝的查询不应返回部分台账视图: %+v", v)
		}
	}
	// 用原编号、原内容重放损坏的请求：不得把损坏结果当成成功结果返回。
	if _, err := s.RegisterRecipe("r1", "R1", "v1", "配方一", []MaterialInput{
		{MaterialNo: "M1", Grams: "1.000"},
		{MaterialNo: "M2", Grams: "0.500"},
	}); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("重放损坏的配方登记请求应返回 ErrCorruptData，得到 %v", err)
	}
	// 对其他正常版本的新写入同样不能绕过。
	if _, err := s.RegisterRecipe("r-new", "R9", "v1", "新配方", []MaterialInput{
		{MaterialNo: "M1", Grams: "1"},
	}); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上登记其他配方应返回 ErrCorruptData，得到 %v", err)
	}

	// 被拒绝的访问不改动文件、不占用请求编号。
	after, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, bad) {
		t.Fatalf("被拒绝的访问不应改动台账文件")
	}
	if bytes.Contains(after, []byte("r-new")) || bytes.Contains(after, []byte("R9")) {
		t.Fatalf("被拒绝的写入不应留下请求记录或业务记录")
	}

	// 恢复后：原成功请求仍取回第一次登记的结果，被拒绝过的编号可正常使用。
	if err := os.WriteFile(filepath.Join(dir, stateFileName), good, 0o644); err != nil {
		t.Fatal(err)
	}
	want := v1ResultView()
	replay, err := s.RegisterRecipe("r1", "R1", "v1", "配方一", []MaterialInput{
		{MaterialNo: "M1", Grams: "1.000"},
		{MaterialNo: "M2", Grams: "0.500"},
	})
	if err != nil {
		t.Fatalf("恢复后重放原成功请求失败: %v", err)
	}
	assertRecipeViewExactly(t, replay, want.RecipeNo, want.Version, want.Name, want.Materials)
	if _, err := s.RegisterRecipe("r-new", "R9", "v1", "新配方", []MaterialInput{
		{MaterialNo: "M1", Grams: "1"},
	}); err != nil {
		t.Fatalf("被拒绝过的请求编号应仍可合法提交: %v", err)
	}
}

// 读取核对的“等值克数”规则不改变请求内容匹配：正常台账中，用同一编号把
// 原来的 1.000 改成数值相等的 1 再提交，仍返回 ErrRequestConflict；原内容
// 重放仍取回第一次的结果，不新增或覆盖版本。
func TestRecipeRequestResultCheckKeepsPayloadMatching(t *testing.T) {
	dir := t.TempDir()
	setupRecipeRequestLedger(t, dir)

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, err := s.RegisterRecipe("r1", "R1", "v1", "配方一", []MaterialInput{
		{MaterialNo: "M1", Grams: "1"},
		{MaterialNo: "M2", Grams: "0.500"},
	}); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("1.000 改成数值相等的 1 应返回 ErrRequestConflict，得到 %v", err)
	}
	replay, err := s.RegisterRecipe("r1", "R1", "v1", "配方一", []MaterialInput{
		{MaterialNo: "M1", Grams: "1.000"},
		{MaterialNo: "M2", Grams: "0.500"},
	})
	if err != nil {
		t.Fatalf("原内容重放应成功: %v", err)
	}
	want := v1ResultView()
	assertRecipeViewExactly(t, replay, want.RecipeNo, want.Version, want.Name, want.Materials)
}
