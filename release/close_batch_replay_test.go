package release

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// 本文件为“关闭批次”（CloseBatch）返回的确认结果补充回归测试。
// 关闭的现有行为：只把执行中的批次变为已关闭，确认已经登记的投料，
// 不以数量吻合作为关闭条件。这里的测试围绕该行为展开：
//   - 关闭结果与随后查询都完整保留批次绑定的配方编号、版本、名称、
//     计划份数，以及关闭前按登记顺序排列的全部投料；
//   - 不同物料穿插登记、填写时间先后不一致时，仍按登记顺序保留序号、
//     数量、时间与登记人，不按时间重排、不把多次投料合成一条；
//   - 逐物料核对保留真实差额，物料之间不互相抵消，也不被改成零；
//   - 调用方拿到的关闭结果是数据副本，修改、追加、清空都不能改写台账，
//     也不能污染同一请求编号重放出的首次原始结果；
//   - 执行中尚无投料的批次仍可关闭（投料列表为空、实投为零、差额为负）；
//     已关闭批次换一个未使用的请求编号再次关闭返回 ErrInvalidState，
//     原有配方、计划、投料与核对结果保持不变。

// registerCloseRecipe 登记关闭回归专用的配方 R2/v1（双物料配方）：
// M1 每份 0.1 克、M2 每份 0.2 克。计划 3 份时应投分别为 0.3 克与 0.6 克，
// 配合下面的投料得到一正一负、绝对值相同的差额（+0.1 与 -0.1），
// 专门用于防止两项差额被互相抵消或被改成零。
func registerCloseRecipe(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.RegisterRecipe("recipe-close", "R2", "v1", "双物料配方", []MaterialInput{
		{MaterialNo: "M1", Grams: "0.1"},
		{MaterialNo: "M2", Grams: "0.2"},
	}); err != nil {
		t.Fatalf("登记 R2/v1 失败: %v", err)
	}
}

// closeFeedBase 是关闭前登记投料使用的固定时间基。
func closeFeedBase() time.Time {
	return time.Date(2026, 10, 2, 14, 0, 0, 0, time.UTC)
}

// wantCloseFeedings 是关闭前三次投料应有的登记顺序内容。
// 登记顺序与填写时间不一致：第 1 条时间最晚，第 2 条时间最早，
// 第 3 条居中。任何按时间重排都会改变这里的先后顺序。
func wantCloseFeedings() []FeedingView {
	base := closeFeedBase()
	return []FeedingView{
		{Seq: 1, MaterialNo: "M1", Grams: "0.1", Time: base.Add(2 * time.Hour), Registrar: "张三"},
		{Seq: 2, MaterialNo: "M2", Grams: "0.5", Time: base, Registrar: "李四"},
		{Seq: 3, MaterialNo: "M1", Grams: "0.3", Time: base.Add(time.Hour), Registrar: "张三"},
	}
}

// feedAndCloseBatch 创建 B1（R2/v1、3 份）并开始执行，穿插登记三次投料
// （M1 两次、M2 一次，填写时间与登记先后不一致），然后用请求编号
// "close-b1" 关闭，返回首次关闭结果。
func feedAndCloseBatch(t *testing.T, s *Store) *BatchView {
	t.Helper()
	if _, err := s.CreateBatch("b1", "B1", "R2", "v1", 3); err != nil {
		t.Fatalf("创建批次失败: %v", err)
	}
	if _, err := s.StartBatch("start-b1", "B1"); err != nil {
		t.Fatalf("开始执行失败: %v", err)
	}
	for i, want := range wantCloseFeedings() {
		if _, err := s.AddFeeding(fmt.Sprintf("f%d", i+1), "B1",
			want.MaterialNo, want.Grams, want.Time, want.Registrar); err != nil {
			t.Fatalf("投料 f%d 失败: %v", i+1, err)
		}
	}
	first, err := s.CloseBatch("close-b1", "B1")
	if err != nil {
		t.Fatalf("首次关闭应成功: %v", err)
	}
	return first
}

// checkFirstCloseView 校验“关闭 B1”首次成功时的完整原始结果：
//   - 保留批次实际绑定的配方编号 R2、版本 v1、名称 双物料配方 与计划份数 3；
//   - 状态为已关闭；
//   - 投料按登记顺序保留 3 条（M1、M2、M1），各自的序号、数量、时间、
//     登记人与登记时一致——不按时间重排，也不把 M1 的两次投料合成一条；
//   - 逐物料核对保留真实差额：M1 应投 0.3、实投 0.4、差额 0.1；
//     M2 应投 0.6、实投 0.5、差额 -0.1。两项差额一正一负，
//     不能互相抵消，也不能被改成零。
func checkFirstCloseView(t *testing.T, v *BatchView) {
	t.Helper()
	if v.BatchNo != "B1" {
		t.Fatalf("批次编号应为 B1，得到 %q", v.BatchNo)
	}
	if v.RecipeNo != "R2" || v.RecipeVersion != "v1" || v.RecipeName != "双物料配方" {
		t.Fatalf("关闭结果应保留绑定的 R2/v1（双物料配方），得到 %q %q %q",
			v.RecipeNo, v.RecipeVersion, v.RecipeName)
	}
	if v.PlannedPortions != 3 {
		t.Fatalf("计划份数应保留为 3，得到 %d", v.PlannedPortions)
	}
	if v.Status != StatusClosed {
		t.Fatalf("关闭结果应为已关闭，得到 %s", v.Status)
	}
	wantFeedings := wantCloseFeedings()
	if len(v.Feedings) != len(wantFeedings) {
		t.Fatalf("关闭结果应保留 %d 条投料（多次投料不能合并），得到 %d 条: %+v",
			len(wantFeedings), len(v.Feedings), v.Feedings)
	}
	for i, want := range wantFeedings {
		f := v.Feedings[i]
		if f.Seq != want.Seq || f.MaterialNo != want.MaterialNo || f.Grams != want.Grams ||
			!f.Time.Equal(want.Time) || f.Registrar != want.Registrar {
			t.Fatalf("第 %d 条投料应按登记顺序保留为 %+v，得到 %+v", i+1, want, f)
		}
	}
	if len(v.Materials) != 2 ||
		v.Materials[0].MaterialNo != "M1" || v.Materials[1].MaterialNo != "M2" {
		t.Fatalf("数量核对应按配方顺序列出 M1、M2 两项，得到 %+v", v.Materials)
	}
	mats := materialsMap(v)
	checkRequirement(t, mats, "M1", "0.3", "0.4", "0.1")  // 0.1×3；实投 0.1+0.3
	checkRequirement(t, mats, "M2", "0.6", "0.5", "-0.1") // 0.2×3；实投 0.5
}

// 关闭成功时，返回结果与随后按批次查询的结果都完整保留配方绑定、计划份数、
// 按登记顺序排列的全部投料与逐物料真实差额；重新打开台账后内容不变。
// 查询返回的视图同样是副本：改动它不影响再次查询到的内容。
func TestCloseBatchResultPreservesFeedingsAndDifferences(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	registerCloseRecipe(t, s)

	first := feedAndCloseBatch(t, s)
	checkFirstCloseView(t, first)

	// 随后按批次查询得到同样的完整内容。
	got := mustGetBatch(t, s, "B1")
	checkFirstCloseView(t, got)

	// 查询结果也是调用方的副本：就地改动后再次查询，内容不受污染。
	got.Status = StatusExecuting
	got.PlannedPortions = 99
	got.Feedings[0].Grams = "999"
	got.Feedings = got.Feedings[:1]
	got.Materials[0].DifferenceGrams = "0"
	checkFirstCloseView(t, mustGetBatch(t, s, "B1"))

	// 重新打开台账后，关闭时保留的内容仍然完整。
	if err := s.Close(); err != nil {
		t.Fatalf("关闭台账失败: %v", err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("重新打开台账失败: %v", err)
	}
	t.Cleanup(func() { s2.Close() })
	checkFirstCloseView(t, mustGetBatch(t, s2, "B1"))
}

// 调用方修改首次关闭结果里的计划、状态、配方绑定、投料内容或物料核对项，
// 甚至追加或清空返回的列表，都不能改写已确认的台账，也不能污染首次成功
// 关闭时保存的结果：用同一请求编号、同一批次重复提交关闭，仍成功返回
// 第一次关闭时的完整原始结果；修改重放结果后再查询、再提交，内容依旧。
func TestCloseBatchReplayResultIsCallerOwnedCopy(t *testing.T) {
	s := openTestStore(t)
	registerCloseRecipe(t, s)
	first := feedAndCloseBatch(t, s)
	checkFirstCloseView(t, first)

	// 随意污染调用方拿到的首次关闭结果：计划、状态、配方绑定、
	// 投料条目、物料核对项，并追加伪造的投料与核对项。
	first.Status = StatusExecuting
	first.PlannedPortions = 99
	first.RecipeNo = "RX"
	first.RecipeVersion = "v9"
	first.RecipeName = "被污染的名称"
	first.Feedings[0].Grams = "999"
	first.Feedings[0].Registrar = "外人"
	first.Feedings = append(first.Feedings, FeedingView{
		Seq: 4, MaterialNo: "M1", Grams: "888", Time: closeFeedBase(), Registrar: "外人",
	})
	first.Materials[0].RequiredGrams = "1"
	first.Materials[0].ActualGrams = "999"
	first.Materials[0].DifferenceGrams = "0"
	first.Materials = append(first.Materials, MaterialRequirement{MaterialNo: "FAKE"})

	// 同一请求编号、同一批次重复提交关闭：成功，且返回第一次关闭时的
	// 完整原始结果，不受刚才的本地修改影响。
	replay, err := s.CloseBatch("close-b1", "B1")
	if err != nil {
		t.Fatalf("污染首次返回对象后重放关闭应成功: %v", err)
	}
	checkFirstCloseView(t, replay)

	// 台账本身也未被污染：查询仍得到已关闭批次的原始内容。
	checkFirstCloseView(t, mustGetBatch(t, s, "B1"))

	// 再污染重放结果——这次直接清空两个返回列表并改动核对项，
	// 然后再提交、再查询，仍各自得到原有内容。
	replay.Status = StatusDraft
	replay.Feedings = nil
	replay.Materials = nil
	replay2, err := s.CloseBatch("close-b1", "B1")
	if err != nil {
		t.Fatalf("清空重放结果列表后再次重放应成功: %v", err)
	}
	checkFirstCloseView(t, replay2)
	checkFirstCloseView(t, mustGetBatch(t, s, "B1"))
}

// 执行中尚无投料的批次仍可关闭：关闭不以数量吻合为条件。
// 关闭结果的投料列表为空，各物料实投为零、差额为负的应投量；
// 同一请求编号重放返回同样的结果，随后查询内容一致。
func TestCloseBatchWithoutFeedings(t *testing.T) {
	s := openTestStore(t)
	registerCloseRecipe(t, s)
	if _, err := s.CreateBatch("b9", "B9", "R2", "v1", 3); err != nil {
		t.Fatalf("创建批次失败: %v", err)
	}
	if _, err := s.StartBatch("start-b9", "B9"); err != nil {
		t.Fatalf("开始执行失败: %v", err)
	}

	checkEmpty := func(v *BatchView) {
		t.Helper()
		if v.BatchNo != "B9" || v.RecipeNo != "R2" || v.RecipeVersion != "v1" ||
			v.RecipeName != "双物料配方" || v.PlannedPortions != 3 {
			t.Fatalf("无投料关闭结果应保留 R2/v1、3 份，得到 %+v", v)
		}
		if v.Status != StatusClosed {
			t.Fatalf("无投料批次关闭后应为已关闭，得到 %s", v.Status)
		}
		if len(v.Feedings) != 0 {
			t.Fatalf("无投料批次的投料列表应为空，得到 %d 条: %+v", len(v.Feedings), v.Feedings)
		}
		mats := materialsMap(v)
		if len(mats) != 2 {
			t.Fatalf("无投料批次仍应列出 M1、M2 两种物料，得到 %d 项: %+v", len(mats), v.Materials)
		}
		checkRequirement(t, mats, "M1", "0.3", "0", "-0.3") // 0.1×3；未投料
		checkRequirement(t, mats, "M2", "0.6", "0", "-0.6") // 0.2×3；未投料
	}

	first, err := s.CloseBatch("close-b9", "B9")
	if err != nil {
		t.Fatalf("执行中无投料的批次应可关闭: %v", err)
	}
	checkEmpty(first)

	// 同一请求编号重放，返回首次关闭时的同一结果。
	replay, err := s.CloseBatch("close-b9", "B9")
	if err != nil {
		t.Fatalf("重放无投料批次的关闭请求应成功: %v", err)
	}
	checkEmpty(replay)

	// 随后查询与关闭结果一致。
	checkEmpty(mustGetBatch(t, s, "B9"))
}

// 已关闭批次换一个未使用的请求编号再次关闭，返回 ErrInvalidState；
// 原有配方绑定、计划份数、投料与核对结果保持不变，失败的请求编号不被占用，
// 首次成功的请求编号仍可重放出原始关闭结果。
func TestCloseBatchClosedBatchNewRequestRejected(t *testing.T) {
	s := openTestStore(t)
	registerCloseRecipe(t, s)
	first := feedAndCloseBatch(t, s)
	checkFirstCloseView(t, first)

	// 新请求编号再次关闭已关闭的 B1 → ErrInvalidState。
	if _, err := s.CloseBatch("close-fresh", "B1"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("新编号关闭已关闭批次应返回 ErrInvalidState，得到 %v", err)
	}
	// 失败的请求编号不被占用：再次使用仍是 ErrInvalidState，而不是冲突。
	if _, err := s.CloseBatch("close-fresh", "B1"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("失败编号不应被占用，再次使用仍应返回 ErrInvalidState，得到 %v", err)
	}

	// 原有配方、计划、投料与核对结果保持不变。
	checkFirstCloseView(t, mustGetBatch(t, s, "B1"))

	// 首次成功的请求编号仍可取得第一次关闭时的完整原始结果。
	replay, err := s.CloseBatch("close-b1", "B1")
	if err != nil {
		t.Fatalf("拒绝后原成功请求仍应可重放: %v", err)
	}
	checkFirstCloseView(t, replay)
}
