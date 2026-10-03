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

func truncateLedger(t *testing.T, dir string) {
	t.Helper()
	if err := os.Truncate(filepath.Join(dir, stateFileName), 0); err != nil {
		t.Fatalf("清空台账文件失败: %v", err)
	}
}

// 已有台账文件但内容为零字节时，无论此前有没有登记过批次，Open 都必须返回
// ErrCorruptData：不返回可用对象，错误说明指向“文件没有内容”，
// 而不是某个批次或配方未找到；原文件保持零字节，不补写空台账。
func TestOpenRejectsZeroByteLedger(t *testing.T) {
	cases := []struct {
		name string
		seed func(t *testing.T, dir string)
	}{
		{
			name: "原本就是空文件",
			seed: func(t *testing.T, dir string) {
				if err := os.WriteFile(filepath.Join(dir, stateFileName), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "登记后被截断",
			seed: func(t *testing.T, dir string) {
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
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tc.seed(t, dir)
			path := filepath.Join(dir, stateFileName)

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
				t.Fatalf("错误应说明台账文件没有内容，得到 %v", err)
			}
			if strings.Contains(msg, "未找到") {
				t.Fatalf("零字节是整份台账损坏，不能报成批次或配方未找到，得到 %v", err)
			}

			// 打开失败不得补写：文件仍为零字节。
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Size() != 0 {
				t.Fatalf("打开失败不应改写零字节台账，当前大小 %d", info.Size())
			}
		})
	}
}

// 目录中没有台账文件仍是首次使用，可正常登记；
// 内容完整可读但没有任何配方或批次的台账也仍可打开。
func TestOpenStillAcceptsAbsentOrEmptyValidLedger(t *testing.T) {
	t.Run("没有台账文件", func(t *testing.T) {
		dir := t.TempDir()
		s, err := Open(dir)
		if err != nil {
			t.Fatalf("首次使用应得到空台账: %v", err)
		}
		defer s.Close()
		if _, err := s.RegisterRecipe("r1", "R1", "v1", "配方一", []MaterialInput{
			{MaterialNo: "M1", Grams: "100"},
		}); err != nil {
			t.Fatalf("首次使用应可登记配方: %v", err)
		}
	})

	t.Run("合法的空台账内容", func(t *testing.T) {
		dir := t.TempDir()
		writeStateFile(t, dir, &persistedState{Version: stateVersion})
		s, err := Open(dir)
		if err != nil {
			t.Fatalf("内容完整的空台账应可打开: %v", err)
		}
		defer s.Close()
		if _, err := s.GetBatch("B1"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("空台账查询批次应返回 ErrNotFound，得到 %v", err)
		}
		if _, err := s.RegisterRecipe("r1", "R1", "v1", "配方一", []MaterialInput{
			{MaterialNo: "M1", Grams: "100"},
		}); err != nil {
			t.Fatalf("合法空台账应可登记配方: %v", err)
		}
	})
}

// 台账成功打开后文件被清空：后续查询与写入都必须按损坏处理，
// 不能返回旧记录、不能把批次当成不存在、不能保存新业务记录或占用请求编号，
// 先前成功请求的幂等重放也不能掩盖本次读取失败；
// 恢复原文件后原数据可查，被拒绝的请求编号仍可合法提交。
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
	if _, err := s.AddFeeding("feed-ok", "B1", "M1", "5", fixedTime, "张三"); err != nil {
		t.Fatal(err)
	}

	// 保存完整台账内容，然后清空文件。
	good, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	truncateLedger(t, dir)

	// 查询不能返回旧记录，也不能把原批次当成不存在。
	if _, err := s.GetBatch("B1"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("清空后查询批次应返回 ErrCorruptData，得到 %v", err)
	}
	if _, err := s.GetRecipe("R1", "v1"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("清空后查询配方应返回 ErrCorruptData，得到 %v", err)
	}

	// 合法写入不能绕过损坏状态；先前已成功的相同请求重放也必须报损坏。
	if _, err := s.AddFeeding("feed-new", "B1", "M1", "1", time.Now(), "李四"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("清空后合法写入应返回 ErrCorruptData，得到 %v", err)
	}
	if _, err := s.AddFeeding("feed-ok", "B1", "M1", "5", fixedTime, "张三"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("清空后幂等重放也应返回 ErrCorruptData，不能用旧结果掩盖，得到 %v", err)
	}
	if _, err := s.RegisterRecipe("r2", "R2", "v1", "配方二", []MaterialInput{
		{MaterialNo: "M9", Grams: "1"},
	}); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("清空后登记配方应返回 ErrCorruptData，得到 %v", err)
	}

	// 被拒绝的写入不保存业务记录、不占用请求编号，文件保持零字节。
	info, err := os.Stat(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Fatalf("被拒绝的写入不应补写台账，当前大小 %d", info.Size())
	}

	// 恢复为原来的完整台账：原配方、批次、投料均可查，
	// 损坏期间被拒绝的请求编号仍可成功，不算冲突。
	if err := os.WriteFile(filepath.Join(dir, stateFileName), good, 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := s.GetBatch("B1")
	if err != nil {
		t.Fatalf("恢复后查询批次应成功: %v", err)
	}
	if len(b.Feedings) != 1 || b.Feedings[0].Grams != "5" {
		t.Fatalf("原投料记录应保留: %+v", b)
	}
	r, err := s.GetRecipe("R1", "v1")
	if err != nil || r.Name != "配方一" {
		t.Fatalf("原配方应可查，得到 %+v, %v", r, err)
	}
	f, err := s.AddFeeding("feed-new", "B1", "M1", "1", time.Now(), "李四")
	if err != nil {
		t.Fatalf("损坏期间被拒绝的请求编号恢复后应可成功，不能误判冲突: %v", err)
	}
	if f.Seq != 2 || f.Grams != "1" {
		t.Fatalf("新投料结果不正确: %+v", f)
	}
	restored, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(restored, []byte("R2")) {
		t.Fatalf("损坏期间被拒绝的配方登记不应留下记录")
	}
}
