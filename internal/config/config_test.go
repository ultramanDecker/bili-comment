package config

import (
	"os"
	"path/filepath"
	"testing"
)

// 把配置目录重定向到临时目录，避免测试碰到用户真实的登录态。
// os.UserConfigDir 在 Windows 上读 %AppData%，在 Linux 上读 $XDG_CONFIG_HOME。
func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("AppData", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	return dir
}

// 首次运行读不到配置文件是正常情况，不能当成错误——
// 否则用户装完还没登录就被一个报错挡住。
func TestLoadMissingFileIsNotAnError(t *testing.T) {
	isolate(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("配置文件不存在时不应报错：%v", err)
	}
	if len(cfg.Cookies) != 0 {
		t.Errorf("期望空配置，实际 %+v", cfg)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	isolate(t)

	want := &Config{Cookies: map[string]string{
		"SESSDATA":   "abc,123,x*31",
		"bili_jct":   "deadbeef",
		"DedeUserID": "42",
	}}
	if err := Save(want); err != nil {
		t.Fatalf("保存失败：%v", err)
	}

	got, err := Load()
	if err != nil {
		t.Fatalf("读取失败：%v", err)
	}
	for k, v := range want.Cookies {
		if got.Cookies[k] != v {
			t.Errorf("cookie %s = %q，期望 %q", k, got.Cookies[k], v)
		}
	}
	if got.SavedAt == "" {
		t.Error("应当记录保存时间")
	}
}

// 登录态等同于账号凭据，落盘权限必须是仅当前用户可读。
// Windows 上 POSIX 权限位不生效，所以只在该平台之外校验。
func TestSaveUsesRestrictivePermissions(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Windows 不支持 POSIX 权限位，落盘权限由用户的 ACL 决定")
	}
	isolate(t)

	if err := Save(&Config{Cookies: map[string]string{"SESSDATA": "x"}}); err != nil {
		t.Fatalf("保存失败：%v", err)
	}

	d, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(d)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("配置目录权限 = %o，期望 700", perm)
	}

	p, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	fi, err = os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("配置文件权限 = %o，期望 600", perm)
	}
}

func TestClear(t *testing.T) {
	isolate(t)

	if err := Save(&Config{Cookies: map[string]string{"SESSDATA": "x"}}); err != nil {
		t.Fatal(err)
	}
	if err := Clear(); err != nil {
		t.Fatalf("清除失败：%v", err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("清除后读取失败：%v", err)
	}
	if len(cfg.Cookies) != 0 {
		t.Errorf("清除后仍读到登录态：%+v", cfg.Cookies)
	}

	// 重复清除应当幂等，方便用户反复执行 logout。
	if err := Clear(); err != nil {
		t.Errorf("重复清除不应报错：%v", err)
	}
}

// 配置文件损坏时给出可操作的提示，而不是一个 JSON 语法错误。
func TestLoadCorruptedFile(t *testing.T) {
	isolate(t)

	p, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("{ 这不是 json"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = Load()
	if err == nil {
		t.Fatal("损坏的配置文件应当报错")
	}
	if !contains(err.Error(), "删除") {
		t.Errorf("错误信息应当告诉用户可以删除配置文件，实际：%v", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
