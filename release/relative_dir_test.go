package release

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// chdir 切换进程工作目录到 dir，并在测试结束时恢复。包内现有测试均为
// 串行执行（没有 t.Parallel），因此临时改变工作目录不会干扰其他用例。
func chdir(t *testing.T, dir string) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("读取当前工作目录失败: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("切换工作目录到 %q 失败: %v", dir, err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
}

// 在甲目录下以相对目录 Open("data") 打开台账并登记配方、批次与投料。
func openLedgerInDirA(t *testing.T, aDir string) *Store {
	t.Helper()
	chdir(t, aDir)
	s, err := Open("data")
	if err != nil {
		t.Fatalf("在甲目录 Open(\"data\") 失败: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	// 打开时即固定为绝对位置，不保留相对目录。
	if want := filepath.Join(aDir, "data"); s.dir != want {
		t.Fatalf("台账目录应固定为 %q，得到 %q", want, s.dir)
	}
	if _, err := s.RegisterRecipe("a-r1", "R1", "v1", "甲配方", []MaterialInput{
		{MaterialNo: "M1", Grams: "100"},
	}); err != nil {
		t.Fatalf("甲台账登记配方失败: %v", err)
	}
	if _, err := s.CreateBatch("a-b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatalf("甲台账创建批次失败: %v", err)
	}
	if _, err := s.StartBatch("a-b2", "B1"); err != nil {
		t.Fatalf("甲台账开始批次失败: %v", err)
	}
	if f, err := s.AddFeeding("a-f1", "B1", "M1", "100.5", time.Now(), "甲"); err != nil {
		t.Fatalf("甲台账登记投料失败: %v", err)
	} else if f.Seq != 1 {
		t.Fatalf("甲台账第一条投料序号应为 1，得到 %d", f.Seq)
	}
	return s
}

// 在乙目录下准备另一个 data 台账：配方编号、版本号、批次编号与甲相同，
// 但每份用量、计划份数、投料完全不同。
func setupLedgerInDirB(t *testing.T, bDir string) {
	t.Helper()
	chdir(t, bDir)
	s, err := Open("data")
	if err != nil {
		t.Fatalf("在乙目录 Open(\"data\") 失败: %v", err)
	}
	defer s.Close()
	if want := filepath.Join(bDir, "data"); s.dir != want {
		t.Fatalf("乙台账目录应固定为 %q，得到 %q", want, s.dir)
	}
	if _, err := s.RegisterRecipe("b-r1", "R1", "v1", "乙配方", []MaterialInput{
		{MaterialNo: "M1", Grams: "7"},
	}); err != nil {
		t.Fatalf("乙台账登记配方失败: %v", err)
	}
	if _, err := s.CreateBatch("b-b1", "B1", "R1", "v1", 3); err != nil {
		t.Fatalf("乙台账创建批次失败: %v", err)
	}
	if _, err := s.StartBatch("b-b2", "B1"); err != nil {
		t.Fatalf("乙台账开始批次失败: %v", err)
	}
	if _, err := s.AddFeeding("b-f1", "B1", "M1", "5", time.Now(), "乙"); err != nil {
		t.Fatalf("乙台账登记投料失败: %v", err)
	}
}

// 用相对目录打开台账后，即使进程工作目录切换到另一个同名台账所在目录，
// 原对象的查询与写入仍必须落在最初打开的位置；在新目录重新 Open 的对象
// 则使用新位置，两者互不影响。台账操作不得改变调用方工作目录。
func TestRelativeDirPinnedAcrossWorkingDirSwitch(t *testing.T) {
	root := t.TempDir()
	aDir := filepath.Join(root, "jia")
	bDir := filepath.Join(root, "yi")
	for _, d := range []string{aDir, bDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	sa := openLedgerInDirA(t, aDir)
	setupLedgerInDirB(t, bDir) // 返回时工作目录已在乙目录

	// 切换工作目录后，原对象查询同名批次仍须得到甲台账的内容。
	chdir(t, bDir)
	got, err := sa.GetBatch("B1")
	if err != nil {
		t.Fatalf("切换工作目录后查询甲台账批次失败: %v", err)
	}
	if got.PlannedPortions != 10 || got.RecipeName != "甲配方" {
		t.Fatalf("原对象查到了乙台账的批次: %+v", got)
	}
	if len(got.Feedings) != 1 || got.Feedings[0].Grams != "100.5" {
		t.Fatalf("甲台账投料记录不正确: %+v", got.Feedings)
	}
	var m1 *MaterialRequirement
	for i := range got.Materials {
		if got.Materials[i].MaterialNo == "M1" {
			m1 = &got.Materials[i]
		}
	}
	if m1 == nil || m1.RequiredGrams != "1000" || m1.ActualGrams != "100.5" || m1.DifferenceGrams != "-899.5" {
		t.Fatalf("甲台账逐物料数量核对不正确: %+v", got.Materials)
	}

	// 台账操作不能改变调用方的工作目录。
	if wd, _ := os.Getwd(); wd != bDir {
		t.Fatalf("查询不应改变工作目录，期望 %q，得到 %q", bDir, wd)
	}

	// 通过原对象追加合法投料，只能更新甲台账：登记序号与累计实投接续甲台账。
	f, err := sa.AddFeeding("a-f2", "B1", "M1", "50", time.Now(), "甲")
	if err != nil {
		t.Fatalf("向甲台账追加投料失败: %v", err)
	}
	if f.Seq != 2 {
		t.Fatalf("追加投料应接续甲台账序号 2，得到 %d", f.Seq)
	}
	got, err = sa.GetBatch("B1")
	if err != nil {
		t.Fatalf("重新查询甲台账批次失败: %v", err)
	}
	if m1 = materialRequirement(got, "M1"); m1.ActualGrams != "150.5" {
		t.Fatalf("甲台账累计实投应为 150.5，得到 %+v", got.Materials)
	}
	if wd, _ := os.Getwd(); wd != bDir {
		t.Fatalf("写入不应改变工作目录，期望 %q，得到 %q", bDir, wd)
	}

	// 在乙目录新打开的 Open("data") 对象使用乙台账，与甲台账对象互不影响。
	sb, err := Open("data")
	if err != nil {
		t.Fatalf("在乙目录新 Open(\"data\") 失败: %v", err)
	}
	defer sb.Close()
	gotB, err := sb.GetBatch("B1")
	if err != nil {
		t.Fatalf("查询乙台账批次失败: %v", err)
	}
	if gotB.PlannedPortions != 3 || gotB.RecipeName != "乙配方" {
		t.Fatalf("新对象应使用乙台账，得到: %+v", gotB)
	}
	if m1 = materialRequirement(gotB, "M1"); m1.RequiredGrams != "21" || m1.ActualGrams != "5" {
		t.Fatalf("乙台账数量核对不正确: %+v", gotB.Materials)
	}
	if len(gotB.Feedings) != 1 || gotB.Feedings[0].Seq != 1 || gotB.Feedings[0].Grams != "5" {
		t.Fatalf("乙台账投料不得被甲台账追加改动: %+v", gotB.Feedings)
	}

	// 切到没有 data 台账的丙目录：原对象仍能读写甲台账，
	// 既不把甲台账视为空台账，也不在丙目录生成任何业务记录。
	cDir := filepath.Join(root, "bing")
	if err := os.MkdirAll(cDir, 0o755); err != nil {
		t.Fatal(err)
	}
	chdir(t, cDir)
	if got, err = sa.GetBatch("B1"); err != nil {
		t.Fatalf("在无台账目录下原对象查询失败: %v", err)
	} else if got.PlannedPortions != 10 {
		t.Fatalf("在无台账目录下不应把甲台账读成空台账: %+v", got)
	}
	if _, err := sa.AddFeeding("a-f3", "B1", "M1", "0.25", time.Now(), "甲"); err != nil {
		t.Fatalf("在无台账目录下向甲台账追加投料失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cDir, "data")); !os.IsNotExist(err) {
		t.Fatalf("不得在丙目录生成台账，Stat 结果: %v", err)
	}
	if got, err = sa.GetBatch("B1"); err != nil {
		t.Fatalf("追加后查询甲台账失败: %v", err)
	} else if m1 = materialRequirement(got, "M1"); m1.ActualGrams != "150.75" {
		t.Fatalf("甲台账累计实投应为 150.75，得到 %+v", got.Materials)
	}

	// 乙台账原有内容从头到尾不得改变。
	chdir(t, bDir)
	sb2, err := Open("data")
	if err != nil {
		t.Fatalf("重新打开乙台账失败: %v", err)
	}
	defer sb2.Close()
	if gotB, err = sb2.GetBatch("B1"); err != nil {
		t.Fatalf("重新查询乙台账失败: %v", err)
	}
	if gotB.PlannedPortions != 3 || len(gotB.Feedings) != 1 ||
		gotB.Feedings[0].Grams != "5" || gotB.Feedings[0].Seq != 1 {
		t.Fatalf("乙台账被甲台账操作改动: %+v / %+v", gotB, gotB.Feedings)
	}
}

// 固定位置不代表固定旧数据：甲台账文件在打开后被改坏时，即使乙台账完整
// 可读，原对象的下一次查询和写入仍须返回 ErrCorruptData，不得回退到乙台账、
// 不得覆盖问题文件或保存本次变更（包括不占用请求编号）。
func TestRelativeDirCorruptOriginNotFallback(t *testing.T) {
	root := t.TempDir()
	aDir := filepath.Join(root, "jia")
	bDir := filepath.Join(root, "yi")
	for _, d := range []string{aDir, bDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	sa := openLedgerInDirA(t, aDir)
	setupLedgerInDirB(t, bDir)

	aLedger := filepath.Join(aDir, "data", stateFileName)
	good, err := os.ReadFile(aLedger)
	if err != nil {
		t.Fatalf("读取甲台账文件失败: %v", err)
	}
	bLedger := filepath.Join(bDir, "data", stateFileName)
	bBefore, err := os.ReadFile(bLedger)
	if err != nil {
		t.Fatalf("读取乙台账文件失败: %v", err)
	}

	// 工作目录切到完整可读的乙台账位置，同时把甲台账文件改成无法解析的内容。
	chdir(t, bDir)
	garbage := []byte("{这不是合法 JSON")
	if err := os.WriteFile(aLedger, garbage, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := sa.GetBatch("B1"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("甲台账损坏后查询应返回 ErrCorruptData，得到 %v", err)
	}
	if _, err := sa.AddFeeding("a-corrupt-f", "B1", "M1", "1", time.Now(), "甲"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("甲台账损坏后写入应返回 ErrCorruptData，得到 %v", err)
	}

	// 问题文件保持损坏内容，不被覆盖，也不留下临时文件。
	if got, err := os.ReadFile(aLedger); err != nil || string(got) != string(garbage) {
		t.Fatalf("损坏的甲台账文件不得被覆盖，得到 %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(aDir, "data", stateFileName+".tmp")); !os.IsNotExist(err) {
		t.Fatalf("被拒绝的写入不得留下临时文件，Stat 结果: %v", err)
	}
	// 乙台账不得被读取或写入。
	if got, err := os.ReadFile(bLedger); err != nil || string(got) != string(bBefore) {
		t.Fatalf("乙台账不得被动过，得到 %q, %v", got, err)
	}
	if sb, err := Open("data"); err != nil {
		t.Fatalf("乙台账应仍可打开: %v", err)
	} else {
		gotB, err := sb.GetBatch("B1")
		if err != nil {
			t.Fatalf("乙台账应仍可查询: %v", err)
		}
		if gotB.PlannedPortions != 3 || gotB.RecipeName != "乙配方" {
			t.Fatalf("乙台账内容不正确: %+v", gotB)
		}
		sb.Close()
	}

	// 恢复甲台账后，被损坏拒绝的那次写入所用请求编号应未被占用，
	// 同一编号可以正常登记成功并接续甲台账序号。
	if err := os.WriteFile(aLedger, good, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := sa.AddFeeding("a-corrupt-f", "B1", "M1", "1", time.Now(), "甲")
	if err != nil {
		t.Fatalf("恢复后被拒绝的请求编号应可正常使用，得到 %v", err)
	}
	if f.Seq != 2 {
		t.Fatalf("恢复后投料应接续甲台账序号 2，得到 %d", f.Seq)
	}
}

func materialRequirement(b *BatchView, materialNo string) *MaterialRequirement {
	for i := range b.Materials {
		if b.Materials[i].MaterialNo == materialNo {
			return &b.Materials[i]
		}
	}
	return nil
}
