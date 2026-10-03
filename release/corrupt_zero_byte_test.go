package release

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 把台账文件截断为零字节，模拟使用过程中被清空。
func truncateLedger(t *testing.T, dir string) {
	t.Helper()
	if err := os.Truncate(filepath.Join(dir, stateFileName), 0); err != nil {
		t.Fatalf("截断台账文件失败: %v", err)
	}
}

// 已有零字节台账文件（该位置从未登记过任何内容）时，Open 必须返回
// ErrCorruptData，错误原因是文件没有内容，而不是配方或批次未找到；
// 不返回可用的台账对象，也不补写文件。
func TestOpenRejectsZeroByteFileWithoutHistory(t *testing.T) {
	dir := t.TempDir()
	if f, err := os.OpenFile(filepath.Join(dir, stateFileName), os.O_CREATE|os.O_WRONLY, 0o644); err != nil {
		t.Fatal(err)
	} else if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	if !errors.Is(err, ErrCorruptData) {
		t.Fatalf("零字节台账应返回 ErrCorruptData，得到 %v", err)
	}
	if s != nil {
		s.Close()
		t.Fatalf("损坏台账不应返回可用的 Store 对象")
	}
	msg := err.Error()
	if !strings.Contains(msg, "没有内容") {
		t.Fatalf("错误信息应说明台账文件没有内容，得到 %v", err)
	}
	if strings.Contains(msg, "未找到") {
		t.Fatalf("零字节是整份台账损坏，不能报成某个批次或配方未找到，得到 %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Fatalf("打开失败后台账文件应保持零字节，大小=%d", info.Size())
	}
}

// 曾成功登记过的台账被清空后，重新 Open 同样按损坏处理，
// 不能凭先前记录还原，也不能补写成合法空台账。
func TestOpenRejectsZeroByteFileWithHistory(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterRecipe("r1", "R1", "v1", "配方一", []MaterialInput{
		{MaterialNo: "M1", Grams: "100"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	truncateLedger(t, dir)

	s2, err := Open(dir)
	if !errors.Is(err, ErrCorruptData) {
		t.Fatalf("被清空的台账应返回 ErrCorruptData，得到 %v", err)
	}
	if s2 != nil {
		s2.Close()
		t.Fatalf("损坏台账不应返回可用的 Store 对象")
	}
	if info, err := os.Stat(filepath.Join(dir, stateFileName)); err != nil || info.Size() != 0 {
		t.Fatalf("打开失败后台账文件应保持零字节，info=%v err=%v", info, err)
	}
}

// 台账打开后文件被清空：下一次查询或写入必须返回 ErrCorruptData。
// 查询不能返回旧记录，也不能把原批次当成不存在；写入即使内容合法、
// 即使是此前已成功的相同请求，也不能绕过损坏状态或重放旧结果；
// 被拒绝的写入不保存业务记录、不占用请求编号，文件保持零字节。
// 恢复原台账后，原配方、批次、投料可查，损坏期间被拒的请求编号仍可成功使用。
func TestZeroByteCorruptionAfterOpenDetectedOnNextAccess(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, err := s.RegisterRecipe("r1", "R1", "v1", "配方一", []MaterialInput{
		{MaterialNo: "M1", Grams: "100"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("s1", "B1"); err != nil {
		t.Fatal(err)
	}
	fixedTime := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	feeding, err := s.AddFeeding("feed-ok", "B1", "M1", "5", fixedTime, "张三")
	if err != nil {
		t.Fatal(err)
	}

	// 保存完整台账的副本，随后清空。
	good, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	truncateLedger(t, dir)

	// 查询原有批次必须报损坏，不能返回打开时读到的旧记录，
	// 也不能把原批次当成不存在。
	if _, err := s.GetBatch("B1"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("清空后查询原批次应返回 ErrCorruptData，得到 %v", err)
	}
	if _, err := s.GetRecipe("R1", "v1"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("清空后查询原配方应返回 ErrCorruptData，得到 %v", err)
	}
	// 查询根本不存在的批次也必须先报损坏，而不是 ErrNotFound。
	if _, err := s.GetBatch("B-never-existed"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上查询任何批次都应返回 ErrCorruptData，得到 %v", err)
	}

	// 合法写入不能绕过损坏状态。
	if _, err := s.AddFeeding("feed-rejected", "B1", "M1", "1", time.Now(), "李四"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上投料应返回 ErrCorruptData，得到 %v", err)
	}
	// 之前已经成功的相同请求也不能用旧结果掩盖本次读取失败。
	replayed, err := s.AddFeeding("feed-ok", "B1", "M1", "5", fixedTime, "张三")
	if !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上的幂等重放应返回 ErrCorruptData，得到 %v (feeding=%+v)", err, replayed)
	}
	// 其他写入操作同理。
	if _, err := s.RegisterRecipe("r2", "R2", "v1", "配方二", []MaterialInput{
		{MaterialNo: "M1", Grams: "1"},
	}); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上登记配方应返回 ErrCorruptData，得到 %v", err)
	}
	if _, err := s.CreateBatch("b2", "B2", "R1", "v1", 1); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上创建批次应返回 ErrCorruptData，得到 %v", err)
	}

	// 被拒绝的写入不改动文件、不留下业务记录。
	after, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 0 {
		t.Fatalf("被拒绝的写入不应补写零字节台账，大小=%d", len(after))
	}

	// 恢复为原来的完整台账：原配方、批次、投料完整可查。
	if err := os.WriteFile(filepath.Join(dir, stateFileName), good, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetBatch("B1")
	if err != nil {
		t.Fatalf("恢复后查询应成功: %v", err)
	}
	if got.Status != StatusExecuting || got.PlannedPortions != 10 {
		t.Fatalf("恢复后的批次状态不正确: %+v", got)
	}
	if len(got.Feedings) != 1 || got.Feedings[0].Grams != "5" {
		t.Fatalf("原有投料记录应保留: %+v", got.Feedings)
	}
	if r, err := s.GetRecipe("R1", "v1"); err != nil || r.Name != "配方一" {
		t.Fatalf("恢复后配方应可查: r=%+v err=%v", r, err)
	}
	// 损坏期间被拒绝的请求没有成功记录，同一编号提交有效内容仍可成功，
	// 不能被误判成请求冲突。
	f, err := s.AddFeeding("feed-rejected", "B1", "M1", "1", time.Now(), "李四")
	if err != nil {
		t.Fatalf("被拒绝过的请求编号应仍可合法使用: %v", err)
	}
	if f.Seq != feeding.Seq+1 || f.Grams != "1" {
		t.Fatalf("新投料结果不正确: %+v", f)
	}

	// 通过全新的 Open 重新打开也应得到同样完整的数据。
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("恢复后重新打开应成功: %v", err)
	}
	defer s2.Close()
	b, err := s2.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Feedings) != 2 {
		t.Fatalf("重新打开后应看到两条投料，得到 %+v", b.Feedings)
	}
}

// 首次使用（没有台账文件）仍可正常登记配方和批次；
// 内容完整但没有任何配方或批次的合法台账也仍可打开。
func TestFirstUseAndValidEmptyLedgerStillWork(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("没有台账文件的位置应得到空台账: %v", err)
	}
	if _, err := s.RegisterRecipe("r1", "R1", "v1", "配方一", []MaterialInput{
		{MaterialNo: "M1", Grams: "100"},
	}); err != nil {
		t.Fatalf("首次使用应能登记配方: %v", err)
	}
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 3); err != nil {
		t.Fatalf("首次使用应能创建批次: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// 手工放置一份结构完整、可正常解析但没有任何配方或批次的台账。
	emptyDir := t.TempDir()
	writeStateFile(t, emptyDir, &persistedState{Version: stateVersion})
	s2, err := Open(emptyDir)
	if err != nil {
		t.Fatalf("内容完整但没有配方批次的台账应能打开: %v", err)
	}
	defer s2.Close()
	if _, err := s2.GetBatch("B1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("合法空台账查询应返回 ErrNotFound，得到 %v", err)
	}
	// 写一次后应生成正常台账内容，而不是受“零字节”规则影响。
	if _, err := s2.RegisterRecipe("r2", "R9", "v1", "配方九", []MaterialInput{
		{MaterialNo: "M1", Grams: "1"},
	}); err != nil {
		t.Fatalf("合法空台账上应能登记配方: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(emptyDir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 || !bytes.Contains(data, []byte("R9")) {
		t.Fatalf("登记后台账应包含新配方内容")
	}
}
