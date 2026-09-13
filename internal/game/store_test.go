package game

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func assertTestDirectoryEntries(t *testing.T, dir string, wantName string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != wantName {
		t.Fatalf("目录应仅保留 %q，实际 %v", wantName, entries)
	}
}

// TestFileStoreRestart 验证落盘、替换保存及重启后继续结算且不重置资源。
func TestFileStoreRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "local-development.json")
	store := FileStore{Path: path}
	clock := &testClock{stamp: testTime()}
	service, err := NewService(testConfig(t), store, clock.now)
	if err != nil {
		t.Fatal(err)
	}
	assertTestStoredState(t, store, service.Snapshot())
	prepareTestShops(t, service)
	clock.set(testTime().Add(12 * time.Second))
	assertTestResult(t, settleTestService(t, service), 20, 2, true)
	before := service.Snapshot()
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	clock.set(testTime().Add(25 * time.Second))
	restartedStore := FileStore{Path: path}
	restarted, err := NewService(testConfig(t), restartedStore, clock.now)
	if err != nil {
		t.Fatalf("从真实文件重启失败：%v", err)
	}
	assertTestState(t, restarted.Snapshot(), before)
	unchanged, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(original, unchanged) {
		t.Fatalf("重启加载不能改写存档：%v", err)
	}
	result := settleTestService(t, restarted)
	assertTestResult(t, result, 25, 3, true)
	if result.State.Coins != 1325 || result.State.VisitorsRemaining != 45 || result.State.NextShopIndex != 0 {
		t.Fatalf("重启后不能重复计算已结算收益：%+v", result.State)
	}
	assertTestStoredState(t, restartedStore, result.State)
	reloaded, err := NewService(testConfig(t), FileStore{Path: path}, clock.now)
	if err != nil {
		t.Fatal(err)
	}
	assertTestState(t, reloaded.Snapshot(), result.State)
	assertTestResult(t, settleTestService(t, reloaded), 0, 0, false)
	assertTestDirectoryEntries(t, filepath.Dir(path), filepath.Base(path))
}

// TestFileStoreMissingSave 验证读取缺失存档返回可识别错误且不创建文件。
func TestFileStoreMissingSave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "missing", "save.json")
	_, err := (FileStore{Path: path}).Load()
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("缺失存档应返回 os.ErrNotExist：%v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("Load 不能创建目录或存档：%v，%v", entries, err)
	}
}

// TestFileStoreCorruptionIsNotOverwritten 验证损坏或无效存档拒绝启动且字节不变。
func TestFileStoreCorruptionIsNotOverwritten(t *testing.T) {
	service, _, _ := newTestService(t)
	state := service.Snapshot()
	valid := string(testJSON(t, state))
	badAccounting := copyTestState(state)
	badAccounting.Coins++
	badSchema := copyTestState(state)
	badSchema.SchemaVersion++
	badRules := copyTestState(state)
	badRules.RulesFingerprint = "old-rules"
	cases := []struct {
		name      string
		body      string
		decodeErr bool
	}{
		{"空文件", "", true},
		{"截断文件", valid[:len(valid)/2], true},
		{"非法JSON", "{broken", true},
		{"未知根字段", strings.TrimSuffix(valid, "}") + `,"unknown":true}`, true},
		{"未知店铺字段", strings.Replace(valid, `"id":"clothing"`, `"id":"clothing","unknown":true`, 1), true},
		{"多个JSON值", valid + "\n{}", true},
		{"尾随垃圾", valid + " garbage", true},
		{"错误字段类型", strings.Replace(valid, `"coins":1280`, `"coins":"1280"`, 1), true},
		{"错误时间格式", strings.Replace(valid, state.LastAccrualAt.Format(time.RFC3339Nano), "bad-time", 1), true},
		{"顶层数组", "[]", true},
		{"null存档", "null", false},
		{"缺失字段", "{}", false},
		{"累计账目损坏", string(testJSON(t, badAccounting)), false},
		{"旧版本存档", string(testJSON(t, badSchema)), false},
		{"规则不兼容", string(testJSON(t, badRules)), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "save.json")
			if err := os.WriteFile(path, []byte(tc.body), 0600); err != nil {
				t.Fatal(err)
			}
			store := FileStore{Path: path}
			_, err := store.Load()
			if (err != nil) != tc.decodeErr {
				t.Fatalf("Load 解码错误 = %v，期望错误=%t", err, tc.decodeErr)
			}
			loaded, err := NewService(testConfig(t), store, testTime)
			if err == nil || loaded != nil {
				t.Fatal("损坏或不兼容存档必须拒绝启动")
			}
			data, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(data, []byte(tc.body)) {
				t.Fatalf("失败加载不能覆盖原存档：%v", err)
			}
			assertTestDirectoryEntries(t, filepath.Dir(path), filepath.Base(path))
		})
	}
}

// TestFileStoreSaveFailurePreservesOriginal 验证编码失败不覆盖旧存档且清理临时文件。
func TestFileStoreSaveFailurePreservesOriginal(t *testing.T) {
	service, _, _ := newTestService(t)
	state := service.Snapshot()
	path := filepath.Join(t.TempDir(), "save.json")
	store := FileStore{Path: path}
	before := copyTestState(state)
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}
	assertTestState(t, state, before)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	invalid := copyTestState(state)
	invalid.LastObservedAt = time.Date(10000, time.January, 1, 0, 0, 0, 0, time.UTC)
	invalidBefore := copyTestState(invalid)
	if err := store.Save(invalid); err == nil {
		t.Fatal("不可编码的年份应导致保存失败")
	}
	assertTestState(t, invalid, invalidBefore)
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, original) {
		t.Fatalf("保存失败必须保留原有文件字节：%v", err)
	}
	assertTestStoredState(t, store, state)
	assertTestDirectoryEntries(t, filepath.Dir(path), filepath.Base(path))
}

// TestFileStoreInvalidPaths 验证路径错误返回失败且不损坏已有文件或目录。
func TestFileStoreInvalidPaths(t *testing.T) {
	service, _, _ := newTestService(t)
	state := service.Snapshot()
	t.Run("空路径", func(t *testing.T) {
		if err := (FileStore{}).Save(state); err == nil {
			t.Fatal("空路径必须拒绝保存")
		}
	})
	t.Run("父路径是普通文件", func(t *testing.T) {
		dir := t.TempDir()
		parent := filepath.Join(dir, "parent")
		original := []byte("保留父文件")
		if err := os.WriteFile(parent, original, 0600); err != nil {
			t.Fatal(err)
		}
		if err := (FileStore{Path: filepath.Join(parent, "save.json")}).Save(state); err == nil {
			t.Fatal("父路径是文件时应保存失败")
		}
		data, err := os.ReadFile(parent)
		if err != nil || !bytes.Equal(data, original) {
			t.Fatalf("失败保存不能覆盖父文件：%v", err)
		}
		assertTestDirectoryEntries(t, dir, filepath.Base(parent))
	})
	t.Run("替换目标是非空目录", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "save.json")
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(path, "keep.txt")
		original := []byte("保留目标目录")
		if err := os.WriteFile(marker, original, 0600); err != nil {
			t.Fatal(err)
		}
		if err := (FileStore{Path: path}).Save(state); err == nil {
			t.Fatal("不能将存档替换到非空目录")
		}
		data, err := os.ReadFile(marker)
		if err != nil || !bytes.Equal(data, original) {
			t.Fatalf("替换失败不能破坏已有目录：%v", err)
		}
		assertTestDirectoryEntries(t, dir, filepath.Base(path))
		assertTestDirectoryEntries(t, path, filepath.Base(marker))
	})
}
