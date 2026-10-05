package release

import (
	"errors"
	"testing"
	"time"
)

// 投料幂等回归：保护已经成功登记的一笔投料——重复提交只能取回第一次的
// 结果，既不能把后来追加的另一笔投料误当成原记录，也不能重复计入实投量；
// 沿用同一请求编号但改动任何一项内容，必须按 ErrRequestConflict 拒绝，
// 而不能靠“改动本身非法”被其他错误代替。

// checkFeedingView 比对一条投料视图的全部公开字段。
func checkFeedingView(t *testing.T, got *FeedingView, seq int, material, grams string, tm time.Time, registrar string) {
	t.Helper()
	if got.Seq != seq || got.MaterialNo != material || got.Grams != grams ||
		!got.Time.Equal(tm) || got.Registrar != registrar {
		t.Fatalf("投料视图应为 序号=%d 物料=%q 克数=%q 时间=%v 登记人=%q，得到 %+v",
			seq, material, grams, tm, registrar, got)
	}
}

// setupTwoExecutingBatches 登记标准配方并创建两个都处于执行中的批次
// （B1、B2 均绑定 R1/v1、10 份），用于验证请求编号在同一台账内跨批次共用。
func setupTwoExecutingBatches(t *testing.T) *Store {
	t.Helper()
	s := openTestStore(t)
	registerStandardRecipe(t, s, "recipe-1") // M1=100, M2=0.5, M3=0.010
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatalf("创建 B1 失败: %v", err)
	}
	if _, err := s.StartBatch("s1", "B1"); err != nil {
		t.Fatalf("开始执行 B1 失败: %v", err)
	}
	if _, err := s.CreateBatch("b2", "B2", "R1", "v1", 10); err != nil {
		t.Fatalf("创建 B2 失败: %v", err)
	}
	if _, err := s.StartBatch("s2", "B2"); err != nil {
		t.Fatalf("开始执行 B2 失败: %v", err)
	}
	return s
}

// 成功登记一笔投料后，用原请求编号与完全相同的内容（批次、物料、克数字符串、
// 投料时间、登记人）再次提交，必须返回第一次成功登记的那条记录。即使其间
// 已经追加了其他投料（包括同物料的更近一笔），返回的序号、物料、数量、时间
// 和登记人仍属于原记录；批次查询中的投料条数、登记顺序与各物料累计实投量
// 也不能因重放发生变化。
func TestAddFeedingReplayReturnsFirstRecord(t *testing.T) {
	s := openExecutingBatch(t)
	t1 := time.Date(2026, 2, 1, 9, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 2, 1, 10, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 2, 1, 11, 0, 0, 0, time.UTC)

	first, err := s.AddFeeding("feed-first", "B1", "M1", "100.5", t1, "张三")
	if err != nil {
		t.Fatalf("第一次投料应成功: %v", err)
	}
	checkFeedingView(t, first, 1, "M1", "100.5", t1, "张三")

	// 其间追加两笔其他投料：另一物料一笔、同物料更近一笔，
	// 用来证明重放不会误取回“最近一笔”。
	if _, err := s.AddFeeding("feed-second", "B1", "M2", "3", t2, "李四"); err != nil {
		t.Fatalf("第二次投料应成功: %v", err)
	}
	if _, err := s.AddFeeding("feed-third", "B1", "M1", "50", t3, "王五"); err != nil {
		t.Fatalf("第三次投料应成功: %v", err)
	}

	// 原编号、原内容重新提交：返回的仍是第一条记录，而不是最近一笔 M1。
	replay, err := s.AddFeeding("feed-first", "B1", "M1", "100.5", t1, "张三")
	if err != nil {
		t.Fatalf("原编号原内容重复提交应成功: %v", err)
	}
	checkFeedingView(t, replay, 1, "M1", "100.5", t1, "张三")

	// 再次重复提交，结果稳定不变。
	replay2, err := s.AddFeeding("feed-first", "B1", "M1", "100.5", t1, "张三")
	if err != nil {
		t.Fatalf("再次重复提交应成功: %v", err)
	}
	checkFeedingView(t, replay2, 1, "M1", "100.5", t1, "张三")

	// 批次查询：仍是 3 条、登记顺序不变；重放没有新增第四条、没有重复计实投。
	got, err := s.GetBatch("B1")
	if err != nil {
		t.Fatalf("查询批次失败: %v", err)
	}
	if len(got.Feedings) != 3 {
		t.Fatalf("重放不应新增投料，应仍为 3 条，得到 %d 条", len(got.Feedings))
	}
	want := []struct {
		seq       int
		material  string
		grams     string
		tm        time.Time
		registrar string
	}{
		{1, "M1", "100.5", t1, "张三"},
		{2, "M2", "3", t2, "李四"},
		{3, "M1", "50", t3, "王五"},
	}
	for i, w := range want {
		checkFeedingView(t, &got.Feedings[i], w.seq, w.material, w.grams, w.tm, w.registrar)
	}
	mats := materialsMap(got)
	// M1 = 100.5 + 50 = 150.5；M2 = 3；M3 无投料仍为 0。
	checkRequirement(t, mats, "M1", "1000", "150.5", "-849.5")
	checkRequirement(t, mats, "M2", "5", "3", "-2")
	checkRequirement(t, mats, "M3", "0.1", "0", "-0.1")
}

// 沿用已成功使用的请求编号，只要批次、物料、克数字符串、投料时间或登记人
// 有一项改变，就必须返回可用 errors.Is 判断的 ErrRequestConflict。这些改动
// 本身都符合投料条件（改投同一配方中的另一物料、换一个合法数量、更换登记人、
// 换投料时间、改投另一个执行中批次），冲突不能靠非法输入被拒绝来冒充；
// 内容不同的提交不能覆盖原投料、追加新投料或占用下一个登记序号。
func TestAddFeedingRequestConflictOnChangedContent(t *testing.T) {
	s := setupTwoExecutingBatches(t)
	t1 := time.Date(2026, 2, 2, 9, 0, 0, 0, time.UTC)

	if _, err := s.AddFeeding("feed-first", "B1", "M1", "100", t1, "张三"); err != nil {
		t.Fatalf("第一次投料应成功: %v", err)
	}
	// 其间另有一笔投料占住序号 2，冲突提交若误登记就会占用序号 3。
	if _, err := s.AddFeeding("feed-other", "B1", "M3", "0.010", t1, "赵六"); err != nil {
		t.Fatalf("追加另一笔投料应成功: %v", err)
	}
	// B2 也先有一笔记录，跨批次冲突后必须保持原样。
	if _, err := s.AddFeeding("feed-b2", "B2", "M2", "2", t1, "李四"); err != nil {
		t.Fatalf("B2 投料应成功: %v", err)
	}

	cases := []struct {
		name      string
		batch     string
		material  string
		grams     string
		tm        time.Time
		registrar string
	}{
		{"改投同配方另一物料", "B1", "M2", "100", t1, "张三"},
		{"换一个合法数量", "B1", "M1", "200", t1, "张三"},
		{"更换登记人", "B1", "M1", "100", t1, "李四"},
		{"更换投料时间", "B1", "M1", "100", t1.Add(time.Hour), "张三"},
		{"改投另一个执行中批次", "B2", "M1", "100", t1, "张三"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.AddFeeding("feed-first", tc.batch, tc.material, tc.grams, tc.tm, tc.registrar)
			if !errors.Is(err, ErrRequestConflict) {
				t.Fatalf("同编号改动%s应返回 ErrRequestConflict，得到 %v", tc.name, err)
			}
		})
	}

	// 全部冲突都不覆盖、不追加、不占序号：B1 仍是 2 条，下一个序号仍为 3。
	got, err := s.GetBatch("B1")
	if err != nil {
		t.Fatalf("查询 B1 失败: %v", err)
	}
	if len(got.Feedings) != 2 {
		t.Fatalf("冲突提交不应追加投料，B1 应仍为 2 条，得到 %d 条", len(got.Feedings))
	}
	checkFeedingView(t, &got.Feedings[0], 1, "M1", "100", t1, "张三")
	checkFeedingView(t, &got.Feedings[1], 2, "M3", "0.01", t1, "赵六")
	mats := materialsMap(got)
	// 原 M1 一笔 100 不被覆盖成 200，也不被重复计入；M3 0.010 显示为 0.01。
	checkRequirement(t, mats, "M1", "1000", "100", "-900")
	checkRequirement(t, mats, "M2", "5", "0", "-5")
	checkRequirement(t, mats, "M3", "0.1", "0.01", "-0.09")

	// B2 已有记录保持原样，跨批次冲突没有在 B2 追加。
	gotB2, err := s.GetBatch("B2")
	if err != nil {
		t.Fatalf("查询 B2 失败: %v", err)
	}
	if len(gotB2.Feedings) != 1 {
		t.Fatalf("跨批次冲突不应在 B2 追加，应仍为 1 条，得到 %d 条", len(gotB2.Feedings))
	}
	checkFeedingView(t, &gotB2.Feedings[0], 1, "M2", "2", t1, "李四")

	// 冲突之后，原内容用原编号仍能取得第一次成功结果。
	replay, err := s.AddFeeding("feed-first", "B1", "M1", "100", t1, "张三")
	if err != nil {
		t.Fatalf("冲突后原编号原内容应仍可重放: %v", err)
	}
	checkFeedingView(t, replay, 1, "M1", "100", t1, "张三")

	// 改用尚未使用的请求编号提交与某个冲突改动相同的合法内容，
	// 属于独立一笔：正常追加、取得连续的下一个序号 3。
	independent, err := s.AddFeeding("feed-fresh", "B1", "M2", "100", t1, "张三")
	if err != nil {
		t.Fatalf("新编号提交合法投料应成功: %v", err)
	}
	checkFeedingView(t, independent, 3, "M2", "100", t1, "张三")
	got, _ = s.GetBatch("B1")
	if len(got.Feedings) != 3 {
		t.Fatalf("独立投料应追加为第 3 条，得到 %d 条", len(got.Feedings))
	}
	mats = materialsMap(got)
	// 只增加本次物料 M2 的实投（0 → 100），M1 仍为 100。
	checkRequirement(t, mats, "M1", "1000", "100", "-900")
	checkRequirement(t, mats, "M2", "5", "100", "95")
}

// 克数的显示格式与请求内容要分清：首次提交“1.000”克，成功结果规范化显示
// 为“1”克；再次提交仍带“1.000”时能取回原结果；改成“1”即使数值相等，
// 因请求字符串不同也属于不同内容，必须按请求冲突拒绝。冲突不追加、不计量，
// 原编号原字符串仍可取回原结果；用新编号提交数值相同的“1”则是独立一笔。
func TestAddFeedingGramsFormatIsRequestContent(t *testing.T) {
	s := openExecutingBatch(t)
	t1 := time.Date(2026, 2, 3, 9, 0, 0, 0, time.UTC)

	first, err := s.AddFeeding("feed-format", "B1", "M1", "1.000", t1, "张三")
	if err != nil {
		t.Fatalf("首次投料应成功: %v", err)
	}
	// 请求里的“1.000”在结果中按规范化格式显示为“1”。
	checkFeedingView(t, first, 1, "M1", "1", t1, "张三")

	// 仍带“1.000”重复提交：请求内容一致，取回第一次结果（显示仍是“1”）。
	replay, err := s.AddFeeding("feed-format", "B1", "M1", "1.000", t1, "张三")
	if err != nil {
		t.Fatalf("原字符串重复提交应成功: %v", err)
	}
	checkFeedingView(t, replay, 1, "M1", "1", t1, "张三")

	// 改成“1”：解析后数值相等，但请求内容不同，必须冲突。
	if _, err := s.AddFeeding("feed-format", "B1", "M1", "1", t1, "张三"); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("数值相等但克数字符串不同应返回 ErrRequestConflict，得到 %v", err)
	}

	// 冲突不追加、不重复计实投：仍只有一笔 1 克。
	got, err := s.GetBatch("B1")
	if err != nil {
		t.Fatalf("查询批次失败: %v", err)
	}
	if len(got.Feedings) != 1 {
		t.Fatalf("冲突提交不应追加投料，应仍为 1 条，得到 %d 条", len(got.Feedings))
	}
	checkFeedingView(t, &got.Feedings[0], 1, "M1", "1", t1, "张三")
	if m := materialsMap(got)["M1"]; m.ActualGrams != "1" {
		t.Fatalf("M1 累计实投应仍为 1，得到 %s", m.ActualGrams)
	}

	// 冲突后原字符串仍能取得第一次成功结果。
	replay2, err := s.AddFeeding("feed-format", "B1", "M1", "1.000", t1, "张三")
	if err != nil {
		t.Fatalf("冲突后原字符串应仍可重放: %v", err)
	}
	checkFeedingView(t, replay2, 1, "M1", "1", t1, "张三")

	// 用尚未使用的请求编号提交数值相同的“1”：表示独立一笔，
	// 正常追加、取得连续序号 2，并只增加 M1 的实投（1 → 2）。
	independent, err := s.AddFeeding("feed-format-new", "B1", "M1", "1", t1, "张三")
	if err != nil {
		t.Fatalf("新编号提交应作为独立投料成功: %v", err)
	}
	checkFeedingView(t, independent, 2, "M1", "1", t1, "张三")
	got, _ = s.GetBatch("B1")
	if len(got.Feedings) != 2 {
		t.Fatalf("独立投料应追加为第 2 条，得到 %d 条", len(got.Feedings))
	}
	if m := materialsMap(got)["M1"]; m.ActualGrams != "2" {
		t.Fatalf("M1 累计实投应为两笔合计 2，得到 %s", m.ActualGrams)
	}
}

// 请求编号在同一台账内共用，不按批次各自分配：一个编号已在 B1 成功用于
// 投料后，另一个同样处于执行中的批次 B2 也不能沿用它登记投料，即使该提交
// 本身完全合法。冲突后两个批次的已有记录都应保持原样；B2 改用新编号即可
// 正常登记，B1 的原编号原内容仍可重放。
func TestAddFeedingRequestNoSharedAcrossBatches(t *testing.T) {
	s := setupTwoExecutingBatches(t)
	t1 := time.Date(2026, 2, 4, 9, 0, 0, 0, time.UTC)

	// B1 成功使用该编号登记一笔。
	if _, err := s.AddFeeding("feed-shared", "B1", "M1", "100", t1, "张三"); err != nil {
		t.Fatalf("B1 投料应成功: %v", err)
	}
	// B2 也已有一笔记录（另一物料、另一登记人）。
	if _, err := s.AddFeeding("feed-b2-first", "B2", "M2", "2", t1, "李四"); err != nil {
		t.Fatalf("B2 投料应成功: %v", err)
	}

	// B2 沿用同一编号提交一笔本身合法的投料（执行中、M1 在配方中、数量合法），
	// 必须按请求冲突拒绝，而不是在 B2 再登记一笔。
	if _, err := s.AddFeeding("feed-shared", "B2", "M1", "100", t1, "张三"); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("跨批次沿用请求编号应返回 ErrRequestConflict，得到 %v", err)
	}

	// 两个批次的已有记录都保持原样。
	gotB1, err := s.GetBatch("B1")
	if err != nil {
		t.Fatalf("查询 B1 失败: %v", err)
	}
	if len(gotB1.Feedings) != 1 {
		t.Fatalf("B1 应仍为 1 条，得到 %d 条", len(gotB1.Feedings))
	}
	checkFeedingView(t, &gotB1.Feedings[0], 1, "M1", "100", t1, "张三")
	gotB2, err := s.GetBatch("B2")
	if err != nil {
		t.Fatalf("查询 B2 失败: %v", err)
	}
	if len(gotB2.Feedings) != 1 {
		t.Fatalf("B2 应仍为 1 条，跨批次冲突不得追加，得到 %d 条", len(gotB2.Feedings))
	}
	checkFeedingView(t, &gotB2.Feedings[0], 1, "M2", "2", t1, "李四")

	// B2 改用尚未使用的编号提交相同内容，正常追加为 B2 自己序列的第 2 条。
	second, err := s.AddFeeding("feed-b2-second", "B2", "M1", "100", t1, "张三")
	if err != nil {
		t.Fatalf("B2 用新编号投料应成功: %v", err)
	}
	checkFeedingView(t, second, 2, "M1", "100", t1, "张三")
	gotB2, _ = s.GetBatch("B2")
	if len(gotB2.Feedings) != 2 {
		t.Fatalf("B2 追加后应为 2 条，得到 %d 条", len(gotB2.Feedings))
	}
	b2Mats := materialsMap(gotB2)
	// 只增加本次物料 M1 的实投；M2 仍为此前一笔的 2。
	checkRequirement(t, b2Mats, "M1", "1000", "100", "-900")
	checkRequirement(t, b2Mats, "M2", "5", "2", "-3")

	// B1 的原编号原内容仍可重放，仍是 B1 的第 1 条；B1 记录不受 B2 操作影响。
	replay, err := s.AddFeeding("feed-shared", "B1", "M1", "100", t1, "张三")
	if err != nil {
		t.Fatalf("B1 原编号原内容应仍可重放: %v", err)
	}
	checkFeedingView(t, replay, 1, "M1", "100", t1, "张三")
	gotB1, _ = s.GetBatch("B1")
	if len(gotB1.Feedings) != 1 {
		t.Fatalf("B1 应仍为 1 条，得到 %d 条", len(gotB1.Feedings))
	}
}

// 用尚未使用的请求编号提交与已成功投料完全相同的内容，表示独立的一笔投料：
// 应正常追加、取得连续的下一个序号（即使投料时间也完全相同），并只增加该
// 物料的累计实投量，其他物料不变。
func TestAddFeedingSameContentWithNewRequestAppends(t *testing.T) {
	s := openExecutingBatch(t)
	t1 := time.Date(2026, 2, 5, 9, 0, 0, 0, time.UTC)

	first, err := s.AddFeeding("feed-a", "B1", "M1", "100", t1, "张三")
	if err != nil {
		t.Fatalf("第一次投料应成功: %v", err)
	}
	checkFeedingView(t, first, 1, "M1", "100", t1, "张三")
	// 中间另有一笔其他物料，占住序号 2。
	if _, err := s.AddFeeding("feed-b", "B1", "M2", "2", t1, "李四"); err != nil {
		t.Fatalf("第二次投料应成功: %v", err)
	}
	// 先用同编号改动内容制造一次冲突，确认冲突不占序号。
	if _, err := s.AddFeeding("feed-a", "B1", "M1", "101", t1, "张三"); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("同编号不同数量应返回 ErrRequestConflict，得到 %v", err)
	}

	// 新编号、与第一笔完全相同的内容（连投料时间都相同）：独立一笔，序号 3。
	independent, err := s.AddFeeding("feed-c", "B1", "M1", "100", t1, "张三")
	if err != nil {
		t.Fatalf("新编号相同内容应作为独立投料成功: %v", err)
	}
	checkFeedingView(t, independent, 3, "M1", "100", t1, "张三")

	got, err := s.GetBatch("B1")
	if err != nil {
		t.Fatalf("查询批次失败: %v", err)
	}
	if len(got.Feedings) != 3 {
		t.Fatalf("应追加为 3 条，得到 %d 条", len(got.Feedings))
	}
	checkFeedingView(t, &got.Feedings[0], 1, "M1", "100", t1, "张三")
	checkFeedingView(t, &got.Feedings[1], 2, "M2", "2", t1, "李四")
	checkFeedingView(t, &got.Feedings[2], 3, "M1", "100", t1, "张三")
	mats := materialsMap(got)
	// M1 两笔各 100，合计 200；M2 仍为 2，不被影响。
	checkRequirement(t, mats, "M1", "1000", "200", "-800")
	checkRequirement(t, mats, "M2", "5", "2", "-3")

	// 原编号原内容依旧只取回第一笔。
	replay, err := s.AddFeeding("feed-a", "B1", "M1", "100", t1, "张三")
	if err != nil {
		t.Fatalf("原编号原内容应仍可重放: %v", err)
	}
	checkFeedingView(t, replay, 1, "M1", "100", t1, "张三")
}
