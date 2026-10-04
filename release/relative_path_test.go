package release

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 以相对目录 Open 后，台账位置必须固定在“打开那一刻”的工作目录上：
// 之后进程切换工作目录，原对象的查询与保存仍只作用于最初的位置，
// 不能在新位置另造台账，也不能被另一个同名台账“劫持”。
func TestRelativeDirPinnedAtOpen(t *testing.T) {
	root := t.TempDir()
	jia := filepath.Join(root, "甲")
	yi := filepath.Join(root, "乙")
	bing := filepath.Join(root, "丙")
	for _, d := range []string{jia, yi, bing} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWD) })

	// 在甲目录下以相对目录 "data" 打开台账并登记：同名配方/批次在甲乙两处
	// 内容不同，用来辨认对象到底绑定了哪个位置。
	if err := os.Chdir(jia); err != nil {
		t.Fatal(err)
	}
	sJia, err := Open("data")
	if err != nil {
		t.Fatalf("在甲目录打开相对台账失败: %v", err)
	}
	t.Cleanup(func() { sJia.Close() })
	if _, err := sJia.RegisterRecipe("jia-r1", "R1", "v1", "甲配方", []MaterialInput{
		{MaterialNo: "M1", Grams: "100"},
		{MaterialNo: "M2", Grams: "0.5"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := sJia.CreateBatch("jia-b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := sJia.StartBatch("jia-s1", "B1"); err != nil {
		t.Fatal(err)
	}
	jiaFirst, err := sJia.AddFeeding("jia-f1", "B1", "M1", "1000", time.Now(), "甲登记人")
	if err != nil {
		t.Fatal(err)
	}
	if jiaFirst.Seq != 1 {
		t.Fatalf("甲台账第一条投料序号应为 1，得到 %d", jiaFirst.Seq)
	}

	// 切换到乙目录：这里也有一个 data 台账，配方/批次编号相同但内容不同。
	if err := os.Chdir(yi); err != nil {
		t.Fatal(err)
	}
	sYi, err := Open("data")
	if err != nil {
		t.Fatalf("在乙目录打开相对台账失败: %v", err)
	}
	t.Cleanup(func() { sYi.Close() })
	if _, err := sYi.RegisterRecipe("yi-r1", "R1", "v1", "乙配方", []MaterialInput{
		{MaterialNo: "M1", Grams: "200"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := sYi.CreateBatch("yi-b1", "B1", "R1", "v1", 5); err != nil {
		t.Fatal(err)
	}
	if _, err := sYi.StartBatch("yi-s1", "B1"); err != nil {
		t.Fatal(err)
	}
	if _, err := sYi.AddFeeding("yi-f1", "B1", "M1", "7", time.Now(), "乙登记人"); err != nil {
		t.Fatal(err)
	}

	// 工作目录现在是乙目录。通过“在甲目录打开的”对象查询同名批次，
	// 必须得到甲台账的配方、状态、投料与逐物料数量核对，而不是乙台账。
	gotJia, err := sJia.GetBatch("B1")
	if err != nil {
		t.Fatalf("切换目录后经原对象查询批次失败: %v", err)
	}
	if gotJia.RecipeName != "甲配方" || gotJia.PlannedPortions != 10 || gotJia.Status != StatusExecuting {
		t.Fatalf("原对象查到了乙台账而非甲台账: %+v", gotJia)
	}
	if len(gotJia.Materials) != 2 {
		t.Fatalf("甲配方应有 2 种物料（乙配方只有 1 种），得到 %+v", gotJia.Materials)
	}
	jiaM1 := gotJia.Materials[0]
	if jiaM1.RequiredGrams != "1000" || jiaM1.ActualGrams != "1000" || jiaM1.DifferenceGrams != "0" {
		t.Fatalf("甲台账 M1 数量核对不正确: %+v", jiaM1)
	}
	if len(gotJia.Feedings) != 1 || gotJia.Feedings[0].Seq != 1 || gotJia.Feedings[0].Grams != "1000" {
		t.Fatalf("甲台账投料不正确: %+v", gotJia.Feedings)
	}
	gotJiaRecipe, err := sJia.GetRecipe("R1", "v1")
	if err != nil {
		t.Fatal(err)
	}
	if gotJiaRecipe.Name != "甲配方" || gotJiaRecipe.Materials[0].Grams != "100" {
		t.Fatalf("原对象查到了乙台账的配方: %+v", gotJiaRecipe)
	}

	// 通过原对象向执行中的批次追加一次合法投料，只能更新甲台账：
	// 登记序号接续甲台账（2），累计实投也接续甲台账。
	f2, err := sJia.AddFeeding("jia-f2", "B1", "M1", "500", time.Now(), "甲登记人")
	if err != nil {
		t.Fatalf("切换目录后经原对象追加投料失败: %v", err)
	}
	if f2.Seq != 2 {
		t.Fatalf("追加投料序号应接续甲台账为 2，得到 %d", f2.Seq)
	}
	gotJia, err = sJia.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	if gotJia.Materials[0].ActualGrams != "1500" {
		t.Fatalf("甲台账 M1 累计实投应为 1500，得到 %s", gotJia.Materials[0].ActualGrams)
	}

	// 乙台账原有内容不得改变：仍是 5 份、1 条投料、实投 7。
	gotYi, err := sYi.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	if gotYi.RecipeName != "乙配方" || gotYi.PlannedPortions != 5 {
		t.Fatalf("乙台账内容被串改: %+v", gotYi)
	}
	if len(gotYi.Feedings) != 1 || gotYi.Feedings[0].Seq != 1 || gotYi.Feedings[0].Grams != "7" {
		t.Fatalf("乙台账投料被串改: %+v", gotYi.Feedings)
	}
	if gotYi.Materials[0].RequiredGrams != "1000" || gotYi.Materials[0].ActualGrams != "7" {
		t.Fatalf("乙台账数量核对被串改: %+v", gotYi.Materials[0])
	}

	// 乙目录下“重新打开”的相对台账对象必须绑定乙台账，与甲对象互不影响。
	sYi2, err := Open("data")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sYi2.Close() })
	if r, err := sYi2.GetRecipe("R1", "v1"); err != nil || r.Name != "乙配方" {
		t.Fatalf("在乙目录新 Open 的对象应使用乙台账，得到 %+v, err=%v", r, err)
	}

	// 台账操作不能改变调用方的工作目录。
	if wd, _ := os.Getwd(); wd != yi {
		t.Fatalf("台账操作改变了工作目录：现在为 %q，应为 %q", wd, yi)
	}

	// 切换到一个没有 data 台账的目录：原对象仍正常读写最初位置，
	// 不能把已有台账当成空台账，也不能在新位置生成任何业务记录。
	if err := os.Chdir(bing); err != nil {
		t.Fatal(err)
	}
	if got, err := sJia.GetBatch("B1"); err != nil || got.PlannedPortions != 10 {
		t.Fatalf("无台账目录下原对象应仍能读取甲台账，得到 %+v, err=%v", got, err)
	}
	f3, err := sJia.AddFeeding("jia-f3", "B1", "M1", "1", time.Now(), "甲登记人")
	if err != nil {
		t.Fatalf("无台账目录下经原对象保存失败: %v", err)
	}
	if f3.Seq != 3 {
		t.Fatalf("保存仍应接续甲台账序号为 3，得到 %d", f3.Seq)
	}
	if _, err := os.Stat(filepath.Join(bing, "data")); !os.IsNotExist(err) {
		t.Fatalf("不得在新工作目录生成台账目录，stat data 得到 err=%v", err)
	}
	if entries, err := os.ReadDir(bing); err != nil || len(entries) != 0 {
		t.Fatalf("丙目录应保持为空，得到 %v, err=%v", entries, err)
	}

	// 甲台账文件在打开后被改坏：即使当前工作目录下的乙台账完整可读，
	// 原对象的下一次查询或写入仍必须返回 ErrCorruptData，不得回退到乙台账，
	// 不得覆盖问题文件或保存本次变更。
	jiaStatePath := filepath.Join(jia, "data", stateFileName)
	garbage := []byte("{这不是合法JSON")
	if err := os.WriteFile(jiaStatePath, garbage, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := sJia.GetBatch("B1"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("甲台账损坏后查询应返回 ErrCorruptData，得到 %v", err)
	}
	if _, err := sJia.AddFeeding("jia-f4", "B1", "M1", "1", time.Now(), "甲登记人"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("甲台账损坏后写入应返回 ErrCorruptData，得到 %v", err)
	}
	// 问题文件原样保留，没有被空台账或本次变更覆盖，也没有留下临时文件。
	if got, err := os.ReadFile(jiaStatePath); err != nil || string(got) != string(garbage) {
		t.Fatalf("损坏的台账文件不得被覆盖，得到 %q, err=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(jia, "data", stateFileName+".tmp")); !os.IsNotExist(err) {
		t.Fatalf("损坏后不得留下临时文件，stat 得到 err=%v", err)
	}
	// 乙台账依旧完整，没有被这次失败的写入波及。
	if r, err := sYi.GetRecipe("R1", "v1"); err != nil || r.Name != "乙配方" {
		t.Fatalf("甲台账损坏不应波及乙台账，得到 %+v, err=%v", r, err)
	}
}
